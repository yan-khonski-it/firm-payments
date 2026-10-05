-- Payments reference the seeded firms, so remove them first.
DELETE FROM payments
WHERE payer_firm_id IN (1, 2, 3)
   OR payee_firm_id IN (1, 2, 3);

DELETE FROM firms
WHERE id IN (1, 2, 3);
