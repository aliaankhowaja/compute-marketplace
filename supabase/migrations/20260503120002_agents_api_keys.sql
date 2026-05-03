-- Lightning / L402 principals and hashed API keys for credit path

create table public.agents (
  id uuid not null primary key default gen_random_uuid(),
  jwt_sub uuid not null,
  offer_type text not null
    constraint agents_offer_type_check check (offer_type in ('bolt12', 'manual_bolt11')),
  bolt12_offer text,
  balance_msats bigint not null default 0
    constraint agents_balance_msats_check check (balance_msats >= -1000000),
  warning_level text not null default 'ok',
  created_at timestamp with time zone not null default now(),
  constraint agents_jwt_sub_unique unique (jwt_sub),
  constraint agents_bolt12_offer_unique unique (bolt12_offer)
);

create index agents_jwt_sub_idx on public.agents using btree (jwt_sub);

create table public.api_keys (
  id uuid not null primary key default gen_random_uuid(),
  user_id uuid not null references public.users (id) on delete cascade,
  key_hash text not null,
  key_prefix text not null,
  name text not null,
  is_active boolean not null default true,
  created_at timestamp with time zone not null default now(),
  last_used_at timestamp with time zone,
  constraint api_keys_key_hash_unique unique (key_hash)
);

create index api_keys_key_hash_idx on public.api_keys using btree (key_hash);
create index api_keys_user_id_idx on public.api_keys using btree (user_id);
