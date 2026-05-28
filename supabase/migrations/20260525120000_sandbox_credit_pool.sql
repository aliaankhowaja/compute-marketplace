-- Per-sandbox credit pool for credits billing (mirrors prepaid_balance_msats for lightning).
-- Floor of -10,000,000 subcents (= -$1) allows grace period overshoot before hard-pause.
ALTER TABLE public.sandboxes
  ADD COLUMN prepaid_balance_subcents bigint NOT NULL DEFAULT 0
    CONSTRAINT sandboxes_prepaid_balance_subcents_floor CHECK (prepaid_balance_subcents >= -10000000);
