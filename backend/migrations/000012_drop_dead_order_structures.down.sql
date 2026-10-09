-- 回滚 000012：恢复 prepay_id 列、其 CHECK 约束与冗余条件唯一索引（定义逐字取自 000001 baseline）。
-- 注意：DROP COLUMN 会连带删除仅引用该列的表级约束 orders_prepay_id_check，回滚须一并重建。
ALTER TABLE public.orders ADD COLUMN IF NOT EXISTS prepay_id text;

ALTER TABLE public.orders ADD CONSTRAINT orders_prepay_id_check CHECK (((prepay_id IS NULL) OR (length(prepay_id) <= 128)));

CREATE UNIQUE INDEX IF NOT EXISTS uq_orders_transaction_id_not_null ON public.orders USING btree (transaction_id) WHERE ((transaction_id IS NOT NULL) AND (transaction_id <> ''::text));
