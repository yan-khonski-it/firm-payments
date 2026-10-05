Q: CO: 


Now implement the database transaction logic for the already parsed and validated bulk-payment request.

Start one database transaction:

BEGIN

Collect all unique firm UUIDs involved in the request:
- the payer UUID;
- all payee UUIDs.

Load and lock all corresponding firm rows in one deterministic order:

SELECT id, uuid, balance_cents
FROM firms
WHERE uuid = ANY($1)
ORDER BY id
FOR UPDATE;

All concurrent payment transactions must use the same ordering by `firms.id`.

After the rows are locked:

1. Verify that all requested firms were found.
   If the payer or any payee does not exist, rollback and return an error.

2. Calculate the total amount that must be deducted from the payer.

3. Check the payer's locked `balance_cents`.
   If the balance is less than the total payment amount:
    - rollback the transaction;
    - return the insufficient-funds error;
    - do not modify any balances;
    - do not insert any payment rows.

4. If the payer has enough funds, deduct the total amount from the payer:

UPDATE firms
SET balance_cents = balance_cents - $1
WHERE id = $2;

5. Credit every receiver.

If the same receiver appears multiple times in the request, aggregate its amounts for the balance update.

For example:

A -> B 100
A -> B 200
A -> C 50

balance updates should be:

A -= 350
B += 300
C += 50

Use SQL arithmetic:

UPDATE firms
SET balance_cents = balance_cents + $1
WHERE id = $2;

6. Insert one row into `payments` for every original payment entry.

Duplicate payees must remain separate payment records.

For example:

A -> B 100, "Invoice 1"
A -> B 200, "Invoice 2"

must create two `payments` rows, even though B's balance may be updated once by +300.

7. Commit the transaction:

COMMIT

If any database operation fails before COMMIT, rollback the entire transaction.

The operation must be atomic:
either all balance changes and all payment rows are committed, or none are committed.

Use the existing database library, transaction APIs, domain errors, and project conventions already present in the repository. Do not redesign unrelated code.