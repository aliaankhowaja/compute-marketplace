-- Switch credit storage from "cents" to "subcents" (1 subcent = 1/100,000 of a cent).
-- This aligns internal storage with the rate column (usd_cents_per_second_x100),
-- eliminating integer truncation that caused overcharging on low-cost tiers.

-- users: rename + scale existing balances
ALTER TABLE public.users DROP CONSTRAINT IF EXISTS users_credit_balance_non_negative;
ALTER TABLE public.users RENAME COLUMN credit_balance_usd_cents TO credit_balance_subcents;
UPDATE public.users SET credit_balance_subcents = credit_balance_subcents * 100000;
ALTER TABLE public.users ADD CONSTRAINT users_credit_balance_non_negative CHECK (credit_balance_subcents >= 0);

-- sandboxes: rename + scale existing pool balances
ALTER TABLE public.sandboxes DROP CONSTRAINT IF EXISTS sandboxes_prepaid_balance_usd_cents_floor;
ALTER TABLE public.sandboxes RENAME COLUMN prepaid_balance_usd_cents TO prepaid_balance_subcents;
UPDATE public.sandboxes SET prepaid_balance_subcents = prepaid_balance_subcents * 100000;
ALTER TABLE public.sandboxes ADD CONSTRAINT sandboxes_prepaid_balance_subcents_floor CHECK (prepaid_balance_subcents >= -10000000);

-- credit_transactions: rename + scale
ALTER TABLE public.credit_transactions RENAME COLUMN amount_usd_cents TO amount_subcents;
ALTER TABLE public.credit_transactions RENAME COLUMN balance_after_usd_cents TO balance_after_subcents;
UPDATE public.credit_transactions SET amount_subcents = amount_subcents * 100000, balance_after_subcents = balance_after_subcents * 100000;
