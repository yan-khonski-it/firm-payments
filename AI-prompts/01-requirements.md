# 01-requirements

Q: GPT: Recall dining philosophers problem. Different approaches, the most simple, LR, LLR.
A: Different strategies, how a deadlock can be avoided. Worst case concurrency.

Q: GPT: now, database transactions, when we need to transfer from one account to multiple other accounts.
A: SELECT ORDER BY FOR UPDATE:
```text
SELECT id, balance
FROM accounts
WHERE id IN (3, 9, 17, 25)
ORDER BY id
FOR UPDATE;
```

Q: GPT: We will need to send multiple payments from one account to multiple receivers in the same transaction.
How about alternative approaches: optimistic locking, SERIALIZABLE, and other approaches. their pros and cons.

A: 
SERIALIZABLE - application must handle `serialization_failure`. contention turns into aborted work and retries,
rather than mostly waiting.

Allow deadlocks + retry, bad if deadlocks are frequent, also if we can avoid a deadlock ourselves, it is better to avoid.

Optimistic locking - works best when conflicts are rare.

MY: I will have more details in the README.md file about different approaches.

Q: GPT: given the problem, where an application handles a request that transfers money from one account to multiple
other accounts, which must be an atomic operation. The request format is (`// comments are ignored`):
```json
{
// Identifies the firm that pays.
"payer_firm_uuid":
"3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41"
,
// Each entry is one payment to one firm.
"payments": [
{
// Amount of the payment in US dollars, as a string, with at most
// 2 optional decimal places. Always positive.
"amount":
"1200.75"
,
// Identifies the firm that receives the payment.
"payee_firm_uuid":
"8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10"
,
// Description of the payment.
"description":
"Bookkeeping cleanup, 3 clients"
},
{
"amount":
"300"
,
"payee_firm_uuid":
"e5f18b3c-2a9d-4c07-8e6b-1d4a7f9c3b25"
,
"description":
"Referral fee, 2 clients"
}
]
}
```

The database tables:
### firms
```text

Column
Type
Description

id
INTEGER
Unique internal identifier of thefirm.

name
TEXT
Name of the firm.

balance_cents
INTEGER
Current balance of the firm, in UScents.

uuid
TEXT
Public identifier of the firm, usedin requests
```

### payments
```text
payments
Column
Type
Description

id
INTEGER
Unique identifier of the payment.

payer_firm_id
INTEGER
id of the firms row that pays.

payee_firm_id
INTEGER
id of the firms row that receivesthe payment.

amount_cents
INTEGER
Amount of the payment, in UScents.

description
TEXT
Description of the payment.
```
what is the best approach to avoid deadlocks and ensure data consistency?


A: one DB transaction + pessimistic row locks acquired in a deterministic global order.
```SQL
BEGIN;

SELECT id, balance_cents
FROM firms
WHERE id IN (4, 17, 31)
ORDER BY id
FOR UPDATE;



UPDATE firms
SET balance_cents = balance_cents - 150075
WHERE id = 17;

UPDATE firms
SET balance_cents = balance_cents + 120075
WHERE id = 4;

UPDATE firms
SET balance_cents = balance_cents + 30000
WHERE id = 31;

INSERT INTO payments (...) VALUES (...);
INSERT INTO payments (...) VALUES (...);

COMMIT;
```

If anything fails:
```SQL
ROLLBACK;
```

Then it describes alternative approaches.

Q: Think of HTTP request validation. 
Edge cases regarding the request. As for scalability,
hot sender and hot receiver - we will discuss them later, so skip scalability issues for now.
Think of other things required to do this HTTP end point, which will executed the transactions.
What are other requirements.

A: MY: it gave common best practices for request validation.
The number of payments, valid parameters, lingerer overflow.
It mentioned Transactional DB constraints.

Also, I simplified the task, and excluded same account id as a sender and receiver.
I also excluded idempotency for now. I need somewhere to pass operation id or idempotency key.
I am thinking about using a header, but it will be out of scope of this task, see readme.

Q: give me technical requirements for this task. Focus what is inside in the scope.
A: 
--------

For this endpoint, I would define the requirements like this.

### Functional requirements

- The endpoint accepts one `payer_firm_uuid` and one or more `payments`.
- Each payment must contain:
    - `amount`
    - `payee_firm_uuid`
    - `description`
- All payments in the request must be executed as **one atomic database transaction**.
- The payer is debited by the sum of all payment amounts.
- Each payee is credited by the corresponding amount.
- One row must be inserted into `payments` for every payment in the request.
- If any validation or database operation fails, **no balance changes or payment records are committed**.

### Request validation

`payer_firm_uuid`:

- required;
- must be a string;
- must be a valid UUID;
- must identify an existing firm.

`payments`:

- required;
- must be an array;
- must contain at least one item;
- should have a defined maximum number of items.

For every payment:

`amount`:

- required;
- must be a string;
- must represent a positive USD amount;
- maximum two decimal places;
- must be convertible exactly to integer cents;
- values such as `0`, negative numbers, malformed numbers, excessive decimal precision, `NaN`, `Infinity`, or scientific notation should be rejected;
- individual amounts and the total amount must not overflow the supported integer type.

`payee_firm_uuid`:

- required;
- must be a string;
- must be a valid UUID;
- must identify an existing firm;
- must **not equal `payer_firm_uuid`**;
- duplicate payees are **allowed**.

`description`:

- required;
- must be a string;
- empty string `""` is allowed;
- `null` is not allowed;
- should have a defined maximum length.

### Business rules

- The payer must have enough balance to cover the **total amount of all payments**.
- The payer balance must never become negative.
- Duplicate payees remain separate payment records.

For example:

```text
A -> B $100
A -> B $200
```

creates two `payments` rows, while the net balance change is:

```text
A -= $300
B += $300
```

### Transaction and concurrency requirements

- All affected firms must be handled inside one DB transaction.
- Concurrent requests must not cause:
    - lost updates;
    - double spending;
    - incorrect balances;
    - partial transfers.
- If pessimistic locking is used, collect all unique affected firm IDs:

```text
payer + all payees
```

then sort them by a deterministic global key such as `firms.id`, and acquire row locks in that order.

Example:

```text
IDs: 17, 4, 31, 4
unique -> 17, 4, 31
sorted -> 4, 17, 31
```

Then lock:

```sql
SELECT id, balance_cents
FROM firms
WHERE id IN (...)
ORDER BY id
FOR UPDATE;
```

This minimizes systematic deadlocks.

### Database consistency requirements

The database should enforce important invariants where possible:

```sql
UNIQUE (firms.uuid)

CHECK (firms.balance_cents >= 0)

CHECK (payments.amount_cents > 0)

FOREIGN KEY (payments.payer_firm_id)
    REFERENCES firms(id)

FOREIGN KEY (payments.payee_firm_id)
    REFERENCES firms(id)
```

`firms.uuid` should be indexed because requests resolve firms by UUID.

For monetary values, storing cents as `BIGINT` is safer than 32-bit `INTEGER`.

### Error handling

The endpoint should distinguish at least:

- malformed request;
- invalid UUID;
- payer not found;
- payee not found;
- payer equals payee;
- invalid amount;
- insufficient funds;
- database/transient transaction failure.

Any failure before commit must result in rollback.

### Core invariant

After a successful request:

\[
payer_{after}
=
payer_{before}
-
\sum(payment.amount)
\]

and for each payee:

\[
payee_{after}
=
payee_{before}
+
\sum(amounts\ addressed\ to\ that\ payee)
\]

The key requirement is: **either every debit, credit, and `payments` row is committed together, or nothing is committed.**

--------

## New chat with GPT model
Q: GPT:
can you formulate the task of the assignment that you find in the attached PDF, use simple language
A:
--------

You need to build a small backend service for firms that have balances inside the platform.

One firm can send money to several other firms in one request. The service must check whether the payer has enough money for the whole batch. If not, **none of the payments should happen**. If yes, the service should subtract the total from the payer, add the right amounts to the recipients, save each payment in the database, and return success. TA Firm Payments

For example:

```text
A has $1,000

Request:
A -> B $100
A -> C $200
```

Total is `$300`.

If A has at least `$300`:

```text
A becomes $700
B gets +$100
C gets +$200
```

and two payment records are inserted.

If A has only `$250`, then:

```text
no balances change
no payment records are inserted
return 422
```

The service must also be safe when **several server instances process requests at the same time**, because the assignment explicitly says to assume a load-balanced multi-instance setup and a relational database such as PostgreSQL or MySQL. TA Firm Payments

So the core problem is really:

> Build one HTTP endpoint that performs a bulk money transfer atomically and correctly under concurrency.

The input contains:

```text
payer_firm_uuid
payments[]
```

and each payment contains:

```text
amount
payee_firm_uuid
description
```

Amounts come as strings like `"1200.75"` and are stored in the database as cents. TA Firm Payments

The database has two main tables:

```text
firms
payments
```

`firms` contains the current balance, and `payments` stores each individual transfer. TA Firm Payments

So if the request has:

```text
A -> B $100
A -> B $200
```

you should still store **two payment rows**, because those are two separate payments, even though B's balance increases by `$300` overall. The sample in the assignment shows the same payee appearing more than once. TA Firm Payments

What they are mainly testing is not complicated business logic. They are testing whether you can design this safely:

```text
HTTP request
    ↓
validate input
    ↓
start DB transaction
    ↓
safely lock / coordinate balances
    ↓
check payer has enough
    ↓
update balances
    ↓
insert payment rows
    ↓
commit
    ↓
return 201
```

And if anything fails before commit:

```text
ROLLBACK
```

so nothing is partially applied.

The deliverable is source code, commit history / AI usage logs if you used AI, and a README explaining your solution, assumptions, problems, improvements, and how to run and verify it. TA Firm Payments

So in one sentence:

> **Build a concurrent-safe bulk payment API where one firm pays multiple firms atomically, with all-or-nothing database updates.**

---------


Q: CO:
Given the task `[copy-pasted from above]`, review the requirements [`copy-pasted from the previous chat above`]
A: Yes. Nothing in your requirements contradicts the task...
MY: and some recommendations...