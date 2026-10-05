# 02-api

Q: CO: implement  an API post endpoint that just return 422 for now.
use go lang programming language.
Use this version
go1.26.3
consider using only standard libraries

Q: CO: write the databae migrations that create the database and populate it with seed data.

Q: GTP: review the database migrations

Q: CO: implement an empty API end-point POST to return nothing for now.



Q: CO:
next, now, implement request validation.

do not call the database to check existing ids or sufficient funds. Only validate JSON, payer and payee are different ids, non empty list of receivers, amounts are greater than 0
and other validation rules defined previously

Q: GPT: now, review handler.go and request.go files and their tests.
is the logic related to request validation (we talked above) correctly implemented

note, we still do not have database logic for now, we do not check existence of accounts and if the sender has enough funds

Q: CQ: check the feedback from above. 
MY: I also had to check the feedback, refine some comments in the code to make them shorter.
Then I asked Claude Opus 5.5 to apply the feedback that the CLaude and I agreed with.


