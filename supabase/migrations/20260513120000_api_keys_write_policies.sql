-- Fix: add INSERT and UPDATE RLS policies for authenticated dashboard users.
-- Addresses silent failure of api_keys creation/revocation from frontend.

create policy api_keys_insert_own
  on public.api_keys
  for insert
  to authenticated
  with check (user_id = auth.uid());

create policy api_keys_update_own
  on public.api_keys
  for update
  to authenticated
  using (user_id = auth.uid())
  with check (user_id = auth.uid());
