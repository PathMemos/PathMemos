
export default Behavior({
  behaviors: [],
  lifetimes: {
    detached() {
      (this as any)._clearTransitionTimers();
    },
  },
  methods: {
    _initTransition(timerName: string = '_timer') {
      const self = this as any;
      if (self[timerName]) clearTimeout(self[timerName]);
      self[timerName] = setTimeout(() => {
        if (self._isDestroyed || self._isDetached) return;
        self._safeSetData({ transition: false });
      }, 300);
    },
    _clearTransitionTimers() {
      const self = this as any;
      if (self._timer) { clearTimeout(self._timer); self._timer = null; }
      if (self._maskTimer) { clearTimeout(self._maskTimer); self._maskTimer = null; }
      // _attachedTimer 一并清理，避免 detached 后 300ms 仍触发一次过渡回调。
      if (self._attachedTimer) { clearTimeout(self._attachedTimer); self._attachedTimer = null; }
    },
    _triggerMaskTransition() {
      const self = this as any;
      if (self._isDestroyed || self._isDetached) return;
      self._safeSetData({ transition: true });
      if (self._maskTimer) clearTimeout(self._maskTimer);
      self._maskTimer = setTimeout(() => {
        if (self._isDestroyed || self._isDetached) return;
        self.triggerEvent('handMask');
      }, 300);
    },
  },
});
