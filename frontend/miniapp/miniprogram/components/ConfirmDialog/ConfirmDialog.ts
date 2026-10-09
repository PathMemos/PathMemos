import i18nBehavior from '../../behaviors/i18n';
import themeBehavior from '../../behaviors/theme';

// root-portal 场景（滚动列表组件内嵌使用）下，本组件被搬到页面根、继承不到
// page/.BasePage 的主题变量，故挂 themeBehavior 自持主题：根节点 data-theme +
// less 内置变量定义，与页面主题经 themeManager 全局同步。
Component({
  behaviors: [themeBehavior, i18nBehavior],
  properties: {
    visible: {
      type: Boolean,
      value: false,
    },
    title: {
      type: String,
      value: '',
      observer(this: any, newVal: any) { if (newVal == null) (this as any)._safeSetData({ title: '' }); },
    },
    content: {
      type: String,
      value: '',
      observer(this: any, newVal: any) { if (newVal == null) (this as any)._safeSetData({ content: '' }); },
    },
    cancelText: {
      type: String,
      value: '',
      observer(this: any, newVal: any) { if (newVal == null) (this as any)._safeSetData({ cancelText: '' }); },
    },
    confirmText: {
      type: String,
      value: '',
      observer(this: any, newVal: any) { if (newVal == null) (this as any)._safeSetData({ confirmText: '' }); },
    },
    confirmType: {
      type: String,
      value: 'default',
    },
    confirmDisabled: {
      type: Boolean,
      value: false,
    },
    showInput: {
      type: Boolean,
      value: false,
    },
    inputDisplayValue: {
      type: String,
      value: '',
      observer(this: any, newVal: any) { if (newVal == null) (this as any)._safeSetData({ inputDisplayValue: '' }); },
    },
    inputValue: {
      type: String,
      value: '',
      observer(this: any, newVal: any) { if (newVal == null) (this as any)._safeSetData({ inputValue: '' }); },
    },
    inputPlaceholder: {
      type: String,
      value: '',
      observer(this: any, newVal: any) { if (newVal == null) (this as any)._safeSetData({ inputPlaceholder: '' }); },
    },
  },

  methods: {
    onMaskTap() {
      this.triggerEvent('close');
    },
    onCancel() {
      this.triggerEvent('cancel');
    },
    onConfirm() {
      if (this.data.confirmDisabled) return;
      this.triggerEvent('confirm');
    },
    onInput(e: any) {
      this.triggerEvent('input', { value: e.detail.value });
    },
  },

  pageLifetimes: {
    hide(this: any) {
      if (this.data.visible) {
        this.triggerEvent('cancel');
      }
    },
  },
});
