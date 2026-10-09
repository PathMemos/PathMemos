#!/usr/bin/env node
// Linux 侧 Donut 多端应用 APK 云构建（miniprogram-ci@beta buildApk）。
// 无需微信开发者工具 IDE：凭证用小程序 CI 上传私钥，签名用本地 keystore，
// 代码包上传微信云端构建后回传 APK（每周额度仅约束本云端构建链路；
// Windows 微信开发者工具本机构建不占该额度、无限量，是额度耗尽期的替代构建通道）。
//
// 流程：暂存工程 → tsc 预编译 TS→ES2018（等价 DevTools 增强编译；CI 自带
// babel 无 optional-chaining 插件，云端编译器不识别 ?. / ??）→ 剥离 .ts 源码
// → CIProject 云构建 APK（含自动签名）。
//
// 用法: node scripts/build-apk-ci.js [仓库frontend/miniapp目录] [版本号] [输出目录] [--clear-cache]
//   版本号缺省取 project.miniapp.json 的 version；输出目录须已存在，产物为
//   <输出目录>/<包名>-<版本>.apk，与 project.miniapp.json 的 versionCode 联动。
//   --clear-cache 显式清空本地基座缓存（默认保留——基座 md5 未变时零额度出包，
//   只有 Android 配置变更才需要重建基座、消耗 1 次额度）。
// 凭证（环境变量可覆盖默认路径）:
//   APK_CI_APPID       多端应用 AppID（默认 wxe56d190a14826b0d）
//   APK_CI_KEY         CI 上传私钥路径（默认 <仓库>/deploy/private.<appid>.key）
//   APK_CI_DEPS        依赖目录（默认 /root/DeployOps/apk-ci-deps，首跑自动 npm i）
//   APK_CI_KEYSTORE    签名库路径（默认 /root/DeployOps/keys/papafeiji.keystore）
//   APK_CI_KSPASS      签名密码（默认读 /root/DeployOps/keys/.ks-pass）
//   APK_CI_STATE_DIR   额度台账目录（默认 /root/DeployOps/apk-ci）
//
// 云构建错误分类与额度台账（固化）:
//   - inner errcode 10002053 = 微信「当周构建次数用尽」（每周免费额度，成功与失败
//     均可能计次）。miniprogram-ci@beta 底层 ciCustomRequest 把非 0 errCode 直接抛出，
//     其 createTask 内的额度降级特判实际不可达（死代码），因此该码只能在本脚本识别。
//     症状形态：request failed, errCode: -1, errMsg: inner upload cloud build
//     resource pack fail with errcode: 10002053, errmsg: 未知错误
//   - 分类处置：额度类（10002053/「次数已用」）不重试、输出出路；瞬时类（system
//     error / errCode:-1 / 网络超时 / 5xx）自动重试 3 次（退避 30s/90s/180s）；其余
//     视为永久错误直接失败。
//   - 台账：每次尝试 append 到 <STATE_DIR>/build-history.jsonl；启动时按 ISO 周
//     （周一起算，本地口径供参考——微信侧真实计数可能含其它端的调用）打印已用次数，
//     ≥45 次响亮警告。
const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

const REPO_DEFAULT = '/root/Github/PathMemos-SaaS';
const ARGV = process.argv.slice(2).filter((a) => a !== '--clear-cache');
const CLEAR_CACHE = process.argv.slice(2).includes('--clear-cache');
const SRC = ARGV[0] || `${REPO_DEFAULT}/frontend/miniapp`;
// buildApk 会 chdir 进暂存目录，构建后还要按 OUTPUT_DIR 解析产物——先取绝对路径，
// 相对路径参数不至于在 chdir 后解析到 /tmp/apk-stage-* 下。
const OUTPUT_DIR = path.resolve(ARGV[2] || '/tmp/apk-out');
const APPID = process.env.APK_CI_APPID || 'wxe56d190a14826b0d';
const CI_KEY = process.env.APK_CI_KEY || `${REPO_DEFAULT}/deploy/private.${APPID}.key`;
const DEPS_DIR = process.env.APK_CI_DEPS || '/root/DeployOps/apk-ci-deps';
const KEYSTORE = process.env.APK_CI_KEYSTORE || '/root/DeployOps/keys/papafeiji.keystore';
const KSPASS =
  process.env.APK_CI_KSPASS ||
  fs.readFileSync('/root/DeployOps/keys/.ks-pass', 'utf8').trim().replace('KSPASS=', '');
const STAGE = `/tmp/apk-stage-${process.pid}`;
const STATE_DIR = process.env.APK_CI_STATE_DIR || '/root/DeployOps/apk-ci';
const HISTORY = path.join(STATE_DIR, 'build-history.jsonl');
const FREE_QUOTA_WEEKLY = 50;
const QUOTA_WARN_AT = 45;
const TRANSIENT_RETRIES = 3;
const BACKOFF_MS = [30_000, 90_000, 180_000];

// 退出即清理暂存目录：失败/中断（SIGINT/SIGTERM）不再残留 /tmp/apk-stage-*，
// 否则每次失败构建遗留约 5MB，多次累积后需手工清理。
process.on('exit', () => {
  try { fs.rmSync(STAGE, { recursive: true, force: true }); } catch {}
});
process.on('SIGINT', () => process.exit(130));
process.on('SIGTERM', () => process.exit(143));

// ---- 错误分类与额度台账 ----

function classifyBuildError(msg) {
  const m = String(msg || '');
  if (/10002053|次数已用/.test(m)) return 'QUOTA_EXHAUSTED';
  if (/system error|errCode: -1|timed?out|ETIMEDOUT|ECONNRESET|EAI_AGAIN|socket hang up|HTTP 50[23]/i.test(m)) {
    return 'TRANSIENT';
  }
  return 'PERMANENT';
}

function isoWeekStart(now = new Date()) {
  const d = new Date(now);
  const offset = (d.getDay() + 6) % 7; // 周一=0 … 周日=6
  d.setDate(d.getDate() - offset);
  d.setHours(0, 0, 0, 0);
  return d;
}

// 当前 ISO 周（周一起算）的已尝试次数；台账缺失/损坏按 0 计（本地口径，供参考）。
function weekAttemptCount() {
  try {
    const since = isoWeekStart();
    let n = 0;
    for (const line of fs.readFileSync(HISTORY, 'utf8').split('\n')) {
      if (!line.trim()) continue;
      try {
        const e = JSON.parse(line);
        if (e.ts && new Date(e.ts) >= since) n++;
      } catch {}
    }
    return n;
  } catch {
    return 0;
  }
}

function recordAttempt(entry) {
  try {
    fs.mkdirSync(STATE_DIR, { recursive: true });
    fs.appendFileSync(HISTORY, JSON.stringify({ ts: new Date().toISOString(), ...entry }) + '\n');
  } catch (e) {
    console.warn('[warn] 额度台账写入失败（不影响构建）:', e.message);
  }
}

function printQuotaGuidance() {
  console.error('==> 结论：微信 Donut 云构建「当周免费额度」已用尽（inner errcode 10002053）。');
  console.error('    出路：');
  console.error('    ① 等微信侧周期重置（自然周）后重跑 release-apk.sh --skip-deploy；');
  console.error('    ② 转 Windows 本机微信开发者工具「打包生成 APK（仅小程序）」（方式 B）——不消耗次数，但需本地基座有效且包名/版本正确，构建后必须解包校验 manifest；');
  console.error('    ③ 若 /tmp/miniapp/android/*/apk/info.json 存在（本地基座可用）且本次仅改小程序代码，');
  console.error('       基座 md5 命中可零额度出包——若本次 Android 配置变更导致 md5 过期则必须消耗额度。');
}

function ensureDeps() {
  if (fs.existsSync(path.join(DEPS_DIR, 'node_modules', 'miniprogram-ci'))) return;
  console.log(`==> 安装构建依赖到 ${DEPS_DIR}（首次）`);
  fs.mkdirSync(DEPS_DIR, { recursive: true });
  execSync('npm init -y', { cwd: DEPS_DIR, stdio: 'pipe' });
  execSync('npm i miniprogram-ci@beta typescript@5.9 --no-audit --no-fund', {
    cwd: DEPS_DIR,
    stdio: 'inherit',
  });
}

function stage() {
  fs.rmSync(STAGE, { recursive: true, force: true });
  fs.mkdirSync(STAGE, { recursive: true });
  execSync(
    `rsync -a --exclude node_modules --exclude dist --exclude test --exclude .git ${SRC}/ ${STAGE}/`,
    { stdio: 'pipe' }
  );
  // tsc 就地降级语法到 ES2018（?. / ?? 等），类型错误不阻断 emit
  fs.writeFileSync(
    path.join(STAGE, 'tsconfig.ci.json'),
    JSON.stringify(
      {
        extends: './tsconfig.json',
        compilerOptions: { target: 'ES2018', outDir: './miniprogram' },
        include: ['./miniprogram/**/*.ts'],
      },
      null,
      2
    )
  );
  try {
    execSync(`${path.join(DEPS_DIR, 'node_modules/.bin/tsc')} -p tsconfig.ci.json`, {
      cwd: STAGE,
      stdio: 'pipe',
    });
  } catch (e) {
    const tail = String(e.stderr || e.message || '').split('\n').slice(-5).join(' | ');
    console.warn('[warn] tsc 预编译有报错（继续，类型错误不阻断 emit）:', tail);
  }
  execSync(`find ${STAGE}/miniprogram -name '*.ts' -delete`);
  // 源码已无 .ts，移除 typescript 编译插件（保留 less）
  const cfgPath = path.join(STAGE, 'project.config.json');
  const cfg = JSON.parse(fs.readFileSync(cfgPath, 'utf8'));
  cfg.setting = cfg.setting || {};
  cfg.setting.useCompilerPlugins = (cfg.setting.useCompilerPlugins || []).filter(
    (p) => p !== 'typescript'
  );
  fs.writeFileSync(cfgPath, JSON.stringify(cfg, null, 2));
  if (!fs.existsSync(path.join(STAGE, 'miniprogram', 'app.js'))) {
    throw new Error('tsc 预编译未产出 app.js，中止');
  }
}

async function main() {
  ensureDeps();
  const ci = require(path.join(DEPS_DIR, 'node_modules', 'miniprogram-ci'));
  const version =
    ARGV[1] || JSON.parse(fs.readFileSync(path.join(SRC, 'project.miniapp.json'))).version;
  // 产物文件名由微信云构建按开放平台绑定的真实包名生成（绑定前 com.tencent.weauth，
  // 绑定后为登记包名），本地只准备输出目录，构建后按版本号解析实际产物。
  fs.mkdirSync(OUTPUT_DIR, { recursive: true });

  stage();
  const project = new ci.CIProject({
    appid: APPID,
    type: 'miniProgram',
    projectPath: STAGE,
    privateKeyPath: CI_KEY,
    ignores: ['node_modules/**/*'],
  });
  // miniprogram-ci buildApk 会在「进程当前目录」创建一个 MD5 命名的工作/缓存目录，
  // 构建结束留下空壳——发布脚本以仓库根为 cwd 启动，导致仓库根堆积 32 位十六进制
  // 空目录（git 不跟踪空目录，status 也看不见）。切到 STAGE：缓存目录随 STAGE 的
  // 退出清理一并带走。SRC/OUTPUT_DIR 均已在此前按绝对路径消费，chdir 无副作用。
  // 注意：基座缓存在 os.tmpdir()/miniapp（与 cwd 无关），不随本脚本退出清理；
  // 默认保留（--clear-cache 才清），基座 md5 命中时零额度出包。
  process.chdir(STAGE);

  const buildOpts = {
    project,
    version,
    desc: 'linux ci cloud build',
    keyStore: KEYSTORE,
    keyPass: KSPASS,
    storePass: KSPASS,
    keyAlias: 'papafeiji',
    output: OUTPUT_DIR,
    useAab: false,
    disableCache: CLEAR_CACHE,
  };

  const used = weekAttemptCount();
  console.log(
    `==> 本周云构建额度（本地台账口径，微信免费 50 次/周，成败均可能计次）：约 ${used} 次已用`
  );
  if (used >= QUOTA_WARN_AT) {
    console.warn(`[warn] 免费额度接近耗尽（本地台账 ${used}/${FREE_QUOTA_WEEKLY}），构建可能被拒`);
  }

  // buildApk 对云端失败不 reject 而是返回 {success:false, errmsg}；重试按分类执行。
  for (let attempt = 1; ; attempt++) {
    const r = await ci.buildApk(buildOpts);
    const built = r.success
      ? fs
          .readdirSync(OUTPUT_DIR)
          .filter((f) => f.endsWith(`-${version}.apk`))
          .map((f) => path.join(OUTPUT_DIR, f))
          .sort((a, b) => fs.statSync(b).mtimeMs - fs.statSync(a).mtimeMs)[0]
      : undefined;
    const cls = r.success ? 'OK' : classifyBuildError(r.errmsg);
    recordAttempt({
      version,
      attempt,
      ok: !!r.success,
      class: cls,
      errmsg: r.success ? undefined : String(r.errmsg || '').slice(0, 200),
    });

    if (r.success && built) {
      console.log('RESULT:', JSON.stringify({ ...r, apk: built, attempt }));
      process.exit(0);
    }
    if (r.success && !built) {
      console.error(`FATAL: 构建报告成功但输出目录未找到版本 ${version} 的产物`);
      console.log('RESULT:', JSON.stringify({ ...r, apk: null, attempt, class: 'NO_ARTIFACT' }));
      process.exit(1);
    }

    const msg = String(r.errmsg || '');
    if (cls === 'QUOTA_EXHAUSTED') {
      console.log('RESULT:', JSON.stringify({ success: false, class: cls, attempt, errmsg: msg }));
      printQuotaGuidance();
      process.exit(1);
    }
    if (cls !== 'TRANSIENT' || attempt > TRANSIENT_RETRIES) {
      console.error(`FATAL: 云构建失败（分类 ${cls}，第 ${attempt} 次尝试）:`, msg);
      if (cls === 'PERMANENT') {
        console.error('    提示：永久类错误请检查版本号递增/证书配置/miniapp json 合法性后再试。');
      } else {
        console.error('    提示：瞬时错误重试仍失败，建议 30-60 分钟后重跑 release-apk.sh --skip-deploy。');
      }
      console.log('RESULT:', JSON.stringify({ success: false, class: cls, attempt, errmsg: msg }));
      process.exit(1);
    }
    const wait = BACKOFF_MS[attempt - 1];
    console.log(`[retry] 瞬时错误（第 ${attempt} 次），${wait / 1000}s 后自动重试...`);
    await new Promise((resolve) => setTimeout(resolve, wait));
  }
}

main().catch((e) => {
  console.error('FATAL:', e && (e.stack || e.message || e));
  process.exit(1);
});
