# 05-refinement.md

Q: CO: some tests failed, check feedback `[pasted from ChatGPT]`.

Q: CO:
Check this feedback

3 — Deadline cancellation can become busy.
lib/pq can return 57014 for a context-triggered
cancellation. [classifyBusy (line 94)] only
examines the returned error, so it can miss the expired context. Check ctx.Err () when classifying a failed operation
and correct the misleading comment. Preserve successful commits and ErrOutcomeUnknown before checking the
context—otherwise the fix could incorrectly claim that nothing was paid.

Check and fix

check these please

Two actionable findings remain:

1. [P2] The request deadline does not reliably bound `BEGIN`.
   [store.go (line 128)] calls `BeginTx`
   before setting database timeouts. The installed `lib/pq` starts watching cancellation only after `BEGIN` completes. I
   reproduced this with a local protocol probe: a 100 ms deadline returned after 1.2 seconds, when the simulated server
   finally closed the connection. A stalled database connection can therefore exceed the 8-second budget and occupy a
   connection. Address this with context-aware driver behavior or connection-level deadlines, backed by a stalled-
   `BEGIN` test.
2. [P2] The unknown-outcome response gives unreliable retry advice.
   [handler.go (line 169)] says “check the
   balances before retrying.” Concurrent payments can change those balances, so they cannot reliably identify whether
   this particular batch committed. Recommend reconciling the payment records and avoiding blind retries. Your decision
   to omit idempotency remains valid for the assignment.
