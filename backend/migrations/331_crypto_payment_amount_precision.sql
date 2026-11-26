-- Adapt source migration 229 for direct crypto receipts. Preserve the
-- existing eighteen integer digits and leave plan and refund scales intact.
ALTER TABLE payment_orders
    ALTER COLUMN amount TYPE NUMERIC(21,3),
    ALTER COLUMN pay_amount TYPE NUMERIC(21,3);
