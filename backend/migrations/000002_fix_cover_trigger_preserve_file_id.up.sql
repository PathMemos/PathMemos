-- manual→default 回退时保留 cover_file_id（可恢复信息），不再连带清空。
CREATE OR REPLACE FUNCTION public.fix_cover_type_on_null_fk() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.manual_cover_file_id IS NULL AND NEW.cover_type = 'manual' THEN
        NEW.cover_type := 'default';
    END IF;
    IF NEW.cover_file_id IS NULL AND NEW.cover_type IN ('image', 'trajectory') THEN
        NEW.cover_type := 'default';
    END IF;
    RETURN NEW;
END;
$$;
