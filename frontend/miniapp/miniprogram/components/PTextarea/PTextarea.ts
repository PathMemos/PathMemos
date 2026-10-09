import { FILE_TYPE } from '../../utils/request';
import i18nBehavior from '../../behaviors/i18n';

Component({
  behaviors: [i18nBehavior],
  properties: {
    value: String,
    imageList: Array,
    maxlength: {
      type: Number,
      value: 10000,
    },
    disabled: {
      type: Boolean,
      value: false,
    },
  },
  data: { data: '', FILE_TYPE },
  observers: {
    value: function (value) {
      const next = value !== undefined && value !== null ? value : '';
      if (this.data.data === next) return;
      (this as any)._safeSetData({ data: next });
    },
  },
  lifetimes: {
    attached() {
      (this as any)._isDetached = false;
    },
    detached() {
      (this as any)._isDetached = true;
    },
  },
  methods: {
    blur() {
      const query = this.createSelectorQuery();
      query.select('#PTextareaInput').node();
      query.exec((res: any) => {
        if ((this as any)._isDetached) return;
        const node = res?.[0]?.node;
        if (node && typeof node.blur === 'function') {
          node.blur();
        }
      });
    },
    onInput(e: any) {
      const value = e.detail.value;
      // 受控回写竞态——父组件把 value 回传（observer）与本地输入可能交错，
      // 值未变化时跳过 setData，避免重复设置 textarea value 导致光标跳动/丢字。
      if (this.data.data !== value) {
        (this as any)._safeSetData({ data: value });
      }
      this.triggerEvent('input', { value });
    },

    handleImage() {
      this.triggerEvent('handleImage');
    },
    handleDel(e: { currentTarget: { dataset: { index: number } } }) {
      const { index } = e.currentTarget.dataset;
      this.triggerEvent('handleDel', { index });
    },
  },
});
