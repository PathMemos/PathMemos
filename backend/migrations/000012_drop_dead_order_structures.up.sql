-- M-07 schema 卫生（docs/MASTER-GOAL）：清理已确认的死结构。
-- 1) uq_orders_transaction_id_not_null 与 orders_transaction_id_key 唯一约束（000001:468）同列冗余：
--    约束覆盖全部 transaction_id 值，条件索引未提供额外语义，仅多一份写放大。
DROP INDEX IF EXISTS public.uq_orders_transaction_id_not_null;

-- 2) orders.prepay_id 无写入点（CreateOrder 恒传空值，恒 NULL）；
--    微信虚拟支付上游交互不落库该字段（payment/service.go requestResponse 为上游 DTO）。
ALTER TABLE public.orders DROP COLUMN IF EXISTS prepay_id;
