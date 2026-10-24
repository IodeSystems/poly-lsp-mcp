CREATE TABLE redline.account (
  account_id bigint NOT NULL,
  email text
);

CREATE OR REPLACE FUNCTION redline.touch() RETURNS TRIGGER AS $$
BEGIN
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE FUNCTION f() RETURNS int AS $body$ SELECT $x$ 1 $x$; $body$ LANGUAGE sql;

ALTER TABLE ONLY redline.account
  ADD CONSTRAINT account_pkey PRIMARY KEY (account_id);
SELECT 'it''s', $$ a $ b $$ FROM x WHERE y = 'z';
