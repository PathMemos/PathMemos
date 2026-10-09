-- 合并基线（2026-08-15）：将历史迁移 000001-000020 合并为单一 baseline，
-- 内容以生产库当前 schema 为准（pg_dump --schema-only）。
-- 注意：不包含 schema_migrations 表（由 migrate CLI / 应用启动器各自创建）。



SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
-- 注意：pg_dump 默认把 search_path 置空；但后续迁移（000003/000004）使用不带 schema 的表名，
-- 置空会让「全新环境」在种子迁移处报 relation does not exist。这里固定为 public。
SELECT pg_catalog.set_config('search_path', 'public', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;





COMMENT ON SCHEMA public IS '';









CREATE FUNCTION public.fix_cover_type_on_null_fk() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    -- manual→default 回退时保留 cover_file_id（可恢复信息），不再连带清空。
    IF NEW.manual_cover_file_id IS NULL AND NEW.cover_type = 'manual' THEN
        NEW.cover_type := 'default';
    END IF;
    IF NEW.cover_file_id IS NULL AND NEW.cover_type IN ('image', 'trajectory') THEN
        NEW.cover_type := 'default';
    END IF;
    RETURN NEW;
END;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;


CREATE TABLE public.ai_daily_quota_usage (
    user_id text NOT NULL,
    quota_date date NOT NULL,
    used integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.ai_dialog_logs (
    id text NOT NULL,
    user_id text NOT NULL,
    role text NOT NULL,
    content text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ai_dialog_logs_role_check CHECK ((role = ANY (ARRAY['user'::text, 'assistant'::text, 'system'::text])))
);



CREATE TABLE public.api_keys (
    id text NOT NULL,
    user_id text NOT NULL,
    key_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    api_key text NOT NULL
);



CREATE TABLE public.auto_record_trajectories (
    id text NOT NULL,
    user_id text NOT NULL,
    lat numeric(10,7) NOT NULL,
    lon numeric(10,7) NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    geocode_attempts integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT auto_record_trajectories_lat_check CHECK (((lat >= ('-90'::integer)::numeric) AND (lat <= (90)::numeric))),
    CONSTRAINT auto_record_trajectories_lon_check CHECK (((lon >= ('-180'::integer)::numeric) AND (lon <= (180)::numeric)))
);



CREATE TABLE public.diaries (
    id text NOT NULL,
    user_id text NOT NULL,
    record_date date NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.diary_entries (
    id text NOT NULL,
    diary_id text NOT NULL,
    created_by text NOT NULL,
    text text,
    lat numeric(10,7),
    lon numeric(10,7),
    address text,
    detail_address text,
    record_time timestamp with time zone,
    sort integer DEFAULT 0 NOT NULL,
    color text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_diary_entries_address_length CHECK (((address IS NULL) OR (length(address) <= 500))),
    CONSTRAINT chk_diary_entries_detail_address_length CHECK (((detail_address IS NULL) OR (length(detail_address) <= 500))),
    CONSTRAINT chk_diary_entries_lat_lon_pair CHECK (((lat IS NULL) = (lon IS NULL))),
    CONSTRAINT diary_entries_color_check CHECK (((color IS NULL) OR (color ~* '^#([0-9A-Fa-f]{3}|[0-9A-Fa-f]{6}|[0-9A-Fa-f]{8})$'::text))),
    CONSTRAINT diary_entries_lat_check CHECK (((lat >= ('-90'::integer)::numeric) AND (lat <= (90)::numeric))),
    CONSTRAINT diary_entries_lon_check CHECK (((lon >= ('-180'::integer)::numeric) AND (lon <= (180)::numeric)))
);



CREATE TABLE public.diary_entry_images (
    id text NOT NULL,
    diary_entry_id text NOT NULL,
    file_id text NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.families (
    id text NOT NULL,
    is_personal boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.family_daily_covers (
    family_id text NOT NULL,
    record_date date NOT NULL,
    cover_file_id text,
    cover_type text DEFAULT 'default'::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    manual_cover_file_id text,
    CONSTRAINT family_daily_covers_cover_type_check CHECK ((cover_type = ANY (ARRAY['image'::text, 'trajectory'::text, 'default'::text, 'manual'::text])))
);



CREATE TABLE public.family_members (
    id text NOT NULL,
    family_id text NOT NULL,
    user_id text NOT NULL,
    role text DEFAULT 'member'::text NOT NULL,
    joined_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT family_members_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'member'::text])))
);



CREATE TABLE public.files (
    id text NOT NULL,
    created_by text,
    path text NOT NULL,
    name text NOT NULL,
    suffix text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL,
    file_type text DEFAULT 'image'::text NOT NULL,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    storage_type text DEFAULT 'local'::text NOT NULL,
    CONSTRAINT files_file_type_check CHECK ((file_type = ANY (ARRAY['image'::text, 'system'::text]))),
    CONSTRAINT files_name_check CHECK ((length(name) <= 255)),
    CONSTRAINT files_path_check CHECK ((length(path) <= 255)),
    CONSTRAINT files_storage_type_check CHECK ((storage_type = ANY (ARRAY['local'::text, 'oss'::text]))),
    CONSTRAINT files_suffix_check CHECK ((length(suffix) <= 32))
);



CREATE TABLE public.memories (
    id text NOT NULL,
    user_id text NOT NULL,
    record_time timestamp with time zone NOT NULL,
    record_date date NOT NULL,
    title text NOT NULL,
    content text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.orders (
    id text NOT NULL,
    user_id text,
    vip_id text NOT NULL,
    out_trade_no text NOT NULL,
    channel text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    amount integer NOT NULL,
    prepay_id text,
    transaction_id text,
    paid_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT orders_amount_check CHECK ((amount > 0)),
    CONSTRAINT orders_channel_check CHECK ((channel = 'virtual_pay'::text)),
    CONSTRAINT orders_out_trade_no_check CHECK ((length(out_trade_no) <= 32)),
    CONSTRAINT orders_prepay_id_check CHECK (((prepay_id IS NULL) OR (length(prepay_id) <= 128))),
    CONSTRAINT orders_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'paid'::text, 'closed'::text]))),
    CONSTRAINT orders_transaction_id_check CHECK (((transaction_id IS NULL) OR (length(transaction_id) <= 128)))
);






CREATE TABLE public.sys_configs (
    id text DEFAULT 'default'::text NOT NULL,
    ai_config jsonb,
    sys_config jsonb,
    default_diary_config jsonb,
    ai_prompt text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT sys_configs_id_check CHECK ((id = 'default'::text))
);



CREATE TABLE public.user_avatar_markers (
    user_id text NOT NULL,
    marker_path text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    storage_type text DEFAULT 'local'::text NOT NULL,
    CONSTRAINT user_avatar_markers_storage_type_check CHECK ((storage_type = ANY (ARRAY['local'::text, 'oss'::text])))
);



CREATE TABLE public.user_common_addresses (
    user_id text NOT NULL,
    name text NOT NULL,
    lat numeric(10,7) NOT NULL,
    lon numeric(10,7) NOT NULL,
    count integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_common_addresses_count_check CHECK ((count >= 0)),
    CONSTRAINT user_common_addresses_lat_check CHECK (((lat >= ('-90'::integer)::numeric) AND (lat <= (90)::numeric))),
    CONSTRAINT user_common_addresses_lon_check CHECK (((lon >= ('-180'::integer)::numeric) AND (lon <= (180)::numeric)))
);



CREATE TABLE public.user_invite_codes (
    user_id text NOT NULL,
    short_code text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    used_at timestamp with time zone
);



CREATE TABLE public.user_invites (
    id text NOT NULL,
    user_id text NOT NULL,
    inviter_id text NOT NULL,
    entry_count integer DEFAULT 0 NOT NULL,
    reward_inviter_at timestamp with time zone,
    reward_invitee_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.user_vip_claims (
    id text NOT NULL,
    user_id text NOT NULL,
    vip_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.user_vips (
    id text NOT NULL,
    user_id text NOT NULL,
    begin_time timestamp with time zone NOT NULL,
    expire_time timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_vips_check CHECK ((expire_time > begin_time))
);



CREATE TABLE public.users (
    id text NOT NULL,
    open_id text NOT NULL,
    unionid text,
    phone_number text,
    avatar text,
    avatar_file_id text,
    nickname text,
    user_type text DEFAULT 'wechat'::text NOT NULL,
    phone_bind_time timestamp with time zone,
    auto_record_enabled boolean DEFAULT false NOT NULL,
    personal_family_id text,
    current_family_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    session_key text,
    invited_by text,
    abnormal_subscribe_accepted boolean DEFAULT false NOT NULL,
    abnormal_alert_sent_at timestamp with time zone,
    last_active_at timestamp with time zone,
    lang text DEFAULT 'zh'::text NOT NULL,
    image_storage_bytes bigint DEFAULT 0 NOT NULL,
    CONSTRAINT users_avatar_check CHECK (((avatar IS NULL) OR (length(avatar) <= 2048))),
    CONSTRAINT users_image_storage_bytes_nonnegative CHECK ((image_storage_bytes >= 0)),
    CONSTRAINT users_user_type_check CHECK ((user_type = 'wechat'::text))
);



CREATE TABLE public.vips (
    id text NOT NULL,
    type text NOT NULL,
    name text NOT NULL,
    time_limit_mark text NOT NULL,
    time_limit_number integer NOT NULL,
    product_id text,
    sort integer DEFAULT 0 NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    prices jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT vips_time_limit_mark_check CHECK ((time_limit_mark = ANY (ARRAY['day'::text, 'month'::text, 'year'::text]))),
    CONSTRAINT vips_time_limit_number_check CHECK ((time_limit_number > 0)),
    CONSTRAINT vips_type_check CHECK ((type = ANY (ARRAY['month'::text, 'year'::text, 'trial'::text, 'free'::text])))
);



CREATE TABLE public.wx_mp_accounts (
    id text NOT NULL,
    user_id text,
    mp_openid text NOT NULL,
    unionid text,
    nickname text,
    avatar text,
    subscribed boolean DEFAULT true NOT NULL,
    subscribe_time timestamp with time zone,
    last_interact_time timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT wx_mp_accounts_avatar_check CHECK (((avatar IS NULL) OR (length(avatar) <= 2048)))
);



ALTER TABLE ONLY public.ai_daily_quota_usage
    ADD CONSTRAINT ai_daily_quota_usage_pkey PRIMARY KEY (user_id, quota_date);



ALTER TABLE ONLY public.ai_dialog_logs
    ADD CONSTRAINT ai_dialog_logs_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_user_id_key UNIQUE (user_id);



ALTER TABLE ONLY public.auto_record_trajectories
    ADD CONSTRAINT auto_record_trajectories_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.diaries
    ADD CONSTRAINT diaries_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.diaries
    ADD CONSTRAINT diaries_user_id_record_date_key UNIQUE (user_id, record_date);



ALTER TABLE ONLY public.diary_entries
    ADD CONSTRAINT diary_entries_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.diary_entry_images
    ADD CONSTRAINT diary_entry_images_diary_entry_id_file_id_key UNIQUE (diary_entry_id, file_id);



ALTER TABLE ONLY public.diary_entry_images
    ADD CONSTRAINT diary_entry_images_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.families
    ADD CONSTRAINT families_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.family_daily_covers
    ADD CONSTRAINT family_daily_covers_pkey PRIMARY KEY (family_id, record_date);



ALTER TABLE ONLY public.family_members
    ADD CONSTRAINT family_members_family_id_user_id_key UNIQUE (family_id, user_id);



ALTER TABLE ONLY public.family_members
    ADD CONSTRAINT family_members_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.files
    ADD CONSTRAINT files_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_out_trade_no_key UNIQUE (out_trade_no);



ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_transaction_id_key UNIQUE (transaction_id);






ALTER TABLE ONLY public.sys_configs
    ADD CONSTRAINT sys_configs_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.family_members
    ADD CONSTRAINT uq_family_members_user_id UNIQUE (user_id) DEFERRABLE INITIALLY DEFERRED;



ALTER TABLE ONLY public.user_avatar_markers
    ADD CONSTRAINT user_avatar_markers_pkey PRIMARY KEY (user_id);



ALTER TABLE ONLY public.user_common_addresses
    ADD CONSTRAINT user_common_addresses_pkey PRIMARY KEY (user_id, name);



ALTER TABLE ONLY public.user_invite_codes
    ADD CONSTRAINT user_invite_codes_pkey PRIMARY KEY (user_id);



ALTER TABLE ONLY public.user_invite_codes
    ADD CONSTRAINT user_invite_codes_short_code_key UNIQUE (short_code);



ALTER TABLE ONLY public.user_invites
    ADD CONSTRAINT user_invites_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.user_invites
    ADD CONSTRAINT user_invites_user_id_key UNIQUE (user_id);



ALTER TABLE ONLY public.user_vip_claims
    ADD CONSTRAINT user_vip_claims_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.user_vip_claims
    ADD CONSTRAINT user_vip_claims_user_id_vip_id_key UNIQUE (user_id, vip_id);



ALTER TABLE ONLY public.user_vips
    ADD CONSTRAINT user_vips_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.user_vips
    ADD CONSTRAINT user_vips_user_id_key UNIQUE (user_id);



ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.vips
    ADD CONSTRAINT vips_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.wx_mp_accounts
    ADD CONSTRAINT wx_mp_accounts_mp_openid_key UNIQUE (mp_openid);



ALTER TABLE ONLY public.wx_mp_accounts
    ADD CONSTRAINT wx_mp_accounts_pkey PRIMARY KEY (id);



CREATE INDEX idx_ai_daily_quota_usage_date ON public.ai_daily_quota_usage USING btree (quota_date);



CREATE INDEX idx_ai_dialog_logs_created_at ON public.ai_dialog_logs USING btree (created_at);



CREATE INDEX idx_ai_dialog_logs_user_created_at ON public.ai_dialog_logs USING btree (user_id, created_at);



CREATE INDEX idx_api_keys_expires_at ON public.api_keys USING btree (expires_at);



CREATE UNIQUE INDEX idx_api_keys_key_hash ON public.api_keys USING btree (key_hash);



CREATE INDEX idx_auto_record_trajectories_created_at ON public.auto_record_trajectories USING btree (created_at);



CREATE INDEX idx_auto_record_trajectories_geocode_attempts ON public.auto_record_trajectories USING btree (geocode_attempts);



CREATE INDEX idx_auto_record_trajectories_recorded_at ON public.auto_record_trajectories USING btree (recorded_at);



CREATE INDEX idx_auto_record_trajectories_user_attempts ON public.auto_record_trajectories USING btree (user_id, geocode_attempts);



CREATE INDEX idx_auto_record_trajectories_user_recorded ON public.auto_record_trajectories USING btree (user_id, recorded_at);



CREATE INDEX idx_diary_entries_auto_last ON public.diary_entries USING btree (created_by, record_time DESC) WHERE (text = '（自动记录）'::text);



CREATE INDEX idx_diary_entries_created_at ON public.diary_entries USING btree (created_at);



CREATE INDEX idx_diary_entries_created_by_created_at ON public.diary_entries USING btree (created_by, created_at);



CREATE INDEX idx_diary_entries_created_by_record_time ON public.diary_entries USING btree (created_by, record_time DESC);



CREATE INDEX idx_diary_entries_creator_address ON public.diary_entries USING btree (created_by, address) WHERE ((address IS NOT NULL) AND (address <> ''::text));



CREATE INDEX idx_diary_entries_diary_created_at ON public.diary_entries USING btree (diary_id, created_at DESC);



CREATE INDEX idx_diary_entries_diary_sort_time ON public.diary_entries USING btree (diary_id, sort, record_time, created_at);



CREATE INDEX idx_diary_entries_updated_at ON public.diary_entries USING btree (updated_at);



CREATE INDEX idx_diary_entry_images_entry_id ON public.diary_entry_images USING btree (diary_entry_id);



CREATE INDEX idx_diary_entry_images_file_id ON public.diary_entry_images USING btree (file_id);



CREATE INDEX idx_family_daily_covers_cover_file_id ON public.family_daily_covers USING btree (cover_file_id) WHERE (cover_file_id IS NOT NULL);



CREATE INDEX idx_family_daily_covers_manual_cover_file_id ON public.family_daily_covers USING btree (manual_cover_file_id) WHERE (manual_cover_file_id IS NOT NULL);



CREATE INDEX idx_family_members_family_role ON public.family_members USING btree (family_id, role);



CREATE INDEX idx_files_created_at ON public.files USING btree (created_at);



CREATE INDEX idx_files_created_by ON public.files USING btree (created_by);



CREATE INDEX idx_files_file_type_created_at ON public.files USING btree (file_type, created_at);



CREATE INDEX idx_files_image_type_id ON public.files USING btree (file_type, id);



CREATE INDEX idx_files_metadata ON public.files USING gin (metadata);



CREATE INDEX idx_files_metadata_family_record ON public.files USING btree (((metadata ->> 'family_id'::text)), ((metadata ->> 'record_date'::text))) WHERE (file_type = 'system'::text);



CREATE INDEX idx_files_path ON public.files USING btree (path);



CREATE INDEX idx_files_storage_type ON public.files USING btree (storage_type);



CREATE INDEX idx_memories_query_covering ON public.memories USING btree (user_id, record_date DESC, record_time DESC) INCLUDE (title, content);



CREATE INDEX idx_orders_null_user ON public.orders USING btree (user_id) WHERE (user_id IS NULL);



CREATE INDEX idx_orders_pending_created_at ON public.orders USING btree (created_at) WHERE (state = 'pending'::text);



CREATE INDEX idx_orders_state_created_at ON public.orders USING btree (state, created_at DESC);



CREATE INDEX idx_orders_user_id_state_created ON public.orders USING btree (user_id, state, created_at);



CREATE INDEX idx_orders_user_vip_state ON public.orders USING btree (user_id, vip_id, state);



CREATE INDEX idx_orders_vip_id ON public.orders USING btree (vip_id);



CREATE INDEX idx_user_invite_codes_short_code_lookup ON public.user_invite_codes USING btree (short_code);



CREATE INDEX idx_user_invites_inviter_created ON public.user_invites USING btree (inviter_id, created_at DESC);



CREATE INDEX idx_user_invites_pending ON public.user_invites USING btree (user_id) WHERE (reward_invitee_at IS NULL);



CREATE INDEX idx_user_invites_reward_inviter_at ON public.user_invites USING btree (inviter_id, reward_inviter_at) WHERE (reward_inviter_at IS NOT NULL);



CREATE INDEX idx_user_vip_claims_user_created ON public.user_vip_claims USING btree (user_id, created_at);



CREATE INDEX idx_user_vip_claims_vip_id ON public.user_vip_claims USING btree (vip_id);



CREATE INDEX idx_user_vips_expire_time ON public.user_vips USING btree (expire_time);



CREATE INDEX idx_users_auto_record_enabled ON public.users USING btree (auto_record_enabled);



CREATE INDEX idx_users_avatar_file_id ON public.users USING btree (avatar_file_id) WHERE (avatar_file_id IS NOT NULL);



CREATE INDEX idx_users_current_family ON public.users USING btree (current_family_id);



CREATE INDEX idx_users_invited_by ON public.users USING btree (invited_by);



CREATE UNIQUE INDEX idx_users_openid ON public.users USING btree (open_id);



CREATE INDEX idx_users_personal_family ON public.users USING btree (personal_family_id);



CREATE UNIQUE INDEX idx_users_phone_number ON public.users USING btree (phone_number) WHERE (phone_number IS NOT NULL);



CREATE UNIQUE INDEX idx_users_unionid ON public.users USING btree (unionid) WHERE (unionid IS NOT NULL);



CREATE INDEX idx_vips_is_active ON public.vips USING btree (is_active);



CREATE UNIQUE INDEX idx_wx_mp_accounts_unionid ON public.wx_mp_accounts USING btree (unionid) WHERE (unionid IS NOT NULL);



CREATE INDEX idx_wx_mp_accounts_user_id ON public.wx_mp_accounts USING btree (user_id);



CREATE UNIQUE INDEX uq_orders_transaction_id_not_null ON public.orders USING btree (transaction_id) WHERE ((transaction_id IS NOT NULL) AND (transaction_id <> ''::text));



CREATE TRIGGER trg_family_daily_covers_fix_type BEFORE INSERT OR UPDATE ON public.family_daily_covers FOR EACH ROW EXECUTE FUNCTION public.fix_cover_type_on_null_fk();



ALTER TABLE ONLY public.ai_dialog_logs
    ADD CONSTRAINT ai_dialog_logs_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.auto_record_trajectories
    ADD CONSTRAINT auto_record_trajectories_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.diaries
    ADD CONSTRAINT diaries_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.diary_entries
    ADD CONSTRAINT diary_entries_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.diary_entries
    ADD CONSTRAINT diary_entries_diary_id_fkey FOREIGN KEY (diary_id) REFERENCES public.diaries(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.diary_entry_images
    ADD CONSTRAINT diary_entry_images_diary_entry_id_fkey FOREIGN KEY (diary_entry_id) REFERENCES public.diary_entries(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.diary_entry_images
    ADD CONSTRAINT diary_entry_images_file_id_fkey FOREIGN KEY (file_id) REFERENCES public.files(id);



ALTER TABLE ONLY public.family_daily_covers
    ADD CONSTRAINT family_daily_covers_cover_file_id_fkey FOREIGN KEY (cover_file_id) REFERENCES public.files(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.family_daily_covers
    ADD CONSTRAINT family_daily_covers_manual_cover_file_id_fkey FOREIGN KEY (manual_cover_file_id) REFERENCES public.files(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.family_members
    ADD CONSTRAINT family_members_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.family_members
    ADD CONSTRAINT family_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.files
    ADD CONSTRAINT files_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.users
    ADD CONSTRAINT fk_users_avatar_file_id FOREIGN KEY (avatar_file_id) REFERENCES public.files(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_vip_id_fkey FOREIGN KEY (vip_id) REFERENCES public.vips(id);



ALTER TABLE ONLY public.user_avatar_markers
    ADD CONSTRAINT user_avatar_markers_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_invite_codes
    ADD CONSTRAINT user_invite_codes_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_invites
    ADD CONSTRAINT user_invites_inviter_id_fkey FOREIGN KEY (inviter_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_invites
    ADD CONSTRAINT user_invites_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_vip_claims
    ADD CONSTRAINT user_vip_claims_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_vip_claims
    ADD CONSTRAINT user_vip_claims_vip_id_fkey FOREIGN KEY (vip_id) REFERENCES public.vips(id);



ALTER TABLE ONLY public.user_vips
    ADD CONSTRAINT user_vips_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_current_family_id_fkey FOREIGN KEY (current_family_id) REFERENCES public.families(id) ON DELETE RESTRICT;



ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_invited_by_fkey FOREIGN KEY (invited_by) REFERENCES public.users(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_personal_family_id_fkey FOREIGN KEY (personal_family_id) REFERENCES public.families(id) ON DELETE RESTRICT;



ALTER TABLE ONLY public.wx_mp_accounts
    ADD CONSTRAINT wx_mp_accounts_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;




