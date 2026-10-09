
import i18nBehavior from '../../behaviors/i18n';
import { i18n } from '../../utils/i18n';

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
    weekdays: [] as string[],
    currentYear: 0,
    currentMonth: 0,
    yearMonthText: '',
    selectedDate: '',
    days: [] as { date: string; day: number; empty: boolean; isToday: boolean; isSelected: boolean }[],
  },

  observers: {
    visible(visible: boolean) {
      if (visible) {
        const value = this.data.value || this.formatDate(new Date());
        const d = this.parseDate(value);
        (this as any)._safeSetData({
          currentYear: d.getFullYear(),
          currentMonth: d.getMonth(),
          selectedDate: value,
          weekdays: (i18n.getMessages().calendarPicker.weekdays || []) as string[],
        }, () => {
          this.buildDays();
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
      (this as any)._safeSetData({
        weekdays: (i18n.getMessages().calendarPicker.weekdays || []) as string[],
      }, () => {
        this.buildDays();
      });
    },

    buildDays() {
      if ((this as any)._isDetached) return;
      const { currentYear, currentMonth, selectedDate } = this.data;
      const firstDay = new Date(currentYear, currentMonth, 1).getDay();
      const daysInMonth = new Date(currentYear, currentMonth + 1, 0).getDate();
      const today = this.formatDate(new Date());

      const days: { date: string; day: number; empty: boolean; isToday: boolean; isSelected: boolean }[] = [];
      for (let i = 0; i < firstDay; i++) {
        days.push({ date: '', day: 0, empty: true, isToday: false, isSelected: false });
      }
      for (let d = 1; d <= daysInMonth; d++) {
        const date = this.formatDate(new Date(currentYear, currentMonth, d));
        days.push({
          date,
          day: d,
          empty: false,
          isToday: date === today,
          isSelected: date === selectedDate,
        });
      }
      const yearMonthText = i18n.t('calendarPicker.yearMonth', { year: currentYear, month: currentMonth + 1 });
      (this as any)._safeSetData({ days, yearMonthText });
    },

    onPrevMonth() {
      if ((this as any)._isDetached) return;
      let { currentYear, currentMonth } = this.data;
      currentMonth--;
      if (currentMonth < 0) {
        currentMonth = 11;
        currentYear--;
      }
      (this as any)._safeSetData({ currentYear, currentMonth }, () => this.buildDays());
    },

    onNextMonth() {
      if ((this as any)._isDetached) return;
      let { currentYear, currentMonth } = this.data;
      currentMonth++;
      if (currentMonth > 11) {
        currentMonth = 0;
        currentYear++;
      }
      (this as any)._safeSetData({ currentYear, currentMonth }, () => this.buildDays());
    },

    onSelectDay(e: any) {
      if ((this as any)._isDetached) return;
      const date = e.currentTarget.dataset.date;
      if (!date) return;
      (this as any)._safeSetData({ selectedDate: date }, () => this.buildDays());
    },

    onConfirm() {
      this.triggerEvent('confirm', { value: this.data.selectedDate });
    },

    onClose() {
      this.triggerEvent('close');
    },

    onMaskTap() {
      this.onClose();
    },

    onSheetTap() {
      
    },

    parseDate(s: string) {
      if (!s) return new Date();
      const [y, m, d] = s.split('-').map(Number);
      const dt = new Date(y, (m || 1) - 1, d || 1);
      // 非法输入产出 Invalid Date 时回退今天，避免 NaN-NaN-NaN 空日历。
      if (isNaN(dt.getTime())) return new Date();
      return dt;
    },

    formatDate(d: Date) {
      const year = d.getFullYear();
      const month = String(d.getMonth() + 1).padStart(2, '0');
      const day = String(d.getDate()).padStart(2, '0');
      return `${year}-${month}-${day}`;
    },
  },
});
