
import i18nBehavior from '../../behaviors/i18n';
import { formatTimeLabel } from '../../utils/util';

Component({
  behaviors: [i18nBehavior],

  properties: {
    visible: {
      type: Boolean,
      value: false,
    },
    value: {
      type: String,
      value: '',
    },
  },

  data: {
    blocks: [] as { time: string; label: string; hour: number }[],
    selectedTime: '',
    selectedHour: -1,
    displayTime: '',
    editVisible: false,
    editHour: '',
    editMinute: '',
  },

  observers: {
    visible(visible: boolean) {
      if (visible) {
        const value = this.data.value || this.formatTime(new Date());
        const [h] = value.split(':').map(Number);
        (this as any)._safeSetData({
          selectedTime: value,
          selectedHour: h,
          displayTime: this.formatDisplayTime(value),
        }, () => {
          this.buildBlocks();
        });
      }
    },
  },

  pageLifetimes: {
    hide(this: any) {
      // 页面切后台时若弹窗打开，统一关闭避免状态残留（AGENTS.md §3.4）。
      if (this.data.visible) {
        this.onClose();
      }
    },
  },

  methods: {
    onLocaleChange() {
      const selectedTime = this.data.selectedTime || this.formatTime(new Date());
      (this as any)._safeSetData({
        displayTime: this.formatDisplayTime(selectedTime),
      }, () => {
        this.buildBlocks();
      });
    },

    buildBlocks() {
      if ((this as any)._isDetached) return;
      const blocks: { time: string; label: string; hour: number }[] = [];
      const order = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 0];
      for (const h of order) {
        const time = `${String(h).padStart(2, '0')}:00`;
        const label = this.formatDisplayLabel(h, 0);
        blocks.push({ time, label, hour: h });
      }
      (this as any)._safeSetData({ blocks });
    },

    onSelectBlock(e: any) {
      if ((this as any)._isDetached) return;
      const time = e.currentTarget.dataset.time;
      const [h] = time.split(':').map(Number);
      (this as any)._safeSetData({
        selectedTime: time,
        selectedHour: h,
        displayTime: this.formatDisplayTime(time),
      });
    },

    onConfirm() {
      this.triggerEvent('confirm', { value: this.data.selectedTime });
    },

    onClose() {
      this.triggerEvent('close');
    },

    onMaskTap() {
      this.onClose();
    },

    // 仅用于拦截弹层冒泡关闭，无业务逻辑。
    onSheetTap() {
      /* 拦截冒泡 */
    },

    onEditTime() {
      if ((this as any)._isDetached) return;
      const [h, m] = this.data.selectedTime.split(':').map(String);
      this.triggerEvent('editOpen');
      (this as any)._safeSetData({
        editVisible: true,
        editHour: h,
        editMinute: m,
      });
    },

    onEditCancel() {
      this.triggerEvent('editClose');
      (this as any)._safeSetData({ editVisible: false });
    },

    onEditMaskTap() {
      this.triggerEvent('editClose');
      this.onEditCancel();
    },

    onEditConfirm() {
      if ((this as any)._isDetached) return;
      let h = parseInt(this.data.editHour || '0', 10);
      let m = parseInt(this.data.editMinute || '0', 10);
      if (Number.isNaN(h)) h = 0;
      if (Number.isNaN(m)) m = 0;
      h = Math.max(0, Math.min(23, h));
      m = Math.max(0, Math.min(59, m));
      const time = `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`;
      this.triggerEvent('editClose');
      (this as any)._safeSetData({
        selectedTime: time,
        selectedHour: h,
        displayTime: this.formatDisplayTime(time),
        editVisible: false,
      });
      this.triggerEvent('confirm', { value: time, fromEdit: true });
    },

    onEditInput(e: any) {
      const field = e.currentTarget.dataset.field;
      const value = (e.detail.value || '').replace(/\D/g, '').slice(0, 2);
      if (field === 'hour') {
        (this as any)._safeSetData({ editHour: value });
      } else if (field === 'minute') {
        (this as any)._safeSetData({ editMinute: value });
      }
    },

    onEditFocus(e: any) {
      const field = e.currentTarget.dataset.field;
      if (field === 'hour') {
        (this as any)._safeSetData({ editHour: '' });
      } else if (field === 'minute') {
        (this as any)._safeSetData({ editMinute: '' });
      }
    },

    // 仅用于拦截弹层冒泡关闭，无业务逻辑。
    onEditSheetTap() {
      /* 拦截冒泡 */
    },

    formatTime(d: Date) {
      const h = String(d.getHours()).padStart(2, '0');
      const m = String(d.getMinutes()).padStart(2, '0');
      return `${h}:${m}`;
    },

    formatDisplayTime(time: string) {
      const [h, m] = time.split(':').map(Number);
      return this.formatDisplayLabel(h, m);
    },

    formatDisplayLabel(h: number, m: number) {
      // 与 NoteEdit 共用统一的时段映射（9-11 上午、12 中午）。
      return formatTimeLabel(h, m);
    },
  },
});
