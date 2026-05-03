-- public.users: one row per dashboard user; id matches auth.users.id

create table public.users (
  id uuid not null primary key references auth.users (id) on delete cascade,
  email text not null,
  credit_balance_usd_cents bigint not null default 0
    constraint users_credit_balance_non_negative check (credit_balance_usd_cents >= 0),
  warning_level text not null default 'ok',
  created_at timestamp with time zone not null default now()
);

create unique index users_email_idx on public.users using btree (email);
