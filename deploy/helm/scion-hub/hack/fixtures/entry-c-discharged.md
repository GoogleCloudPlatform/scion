<!--
FIXTURE, Entry C, the DISCHARGED arm. Not a real VALIDATION.md.

THE WORD "UNRUN" MUST NOT APPEAR ANYWHERE IN THIS FILE, including in this
comment, or the evaluator reads it and the arm inverts. That the fixture's
correctness depends on a word being absent from its own prose is worth stating,
because it is exactly the kind of thing an editor adds back while explaining it.

This fixture carries a RESULT in the failing direction on purpose. The
obligation is discharged by either outcome, and a fixture that only modelled
the happy answer would leave the evaluator's real-world case untested: the
likelier discharge is "we looked, and it is not available here".
-->

#### 7.2 The IAM verification: performed 2026-09-01, IAM DB auth NOT available

The grant landed and the verification was performed. Result: the instance was
created without `cloudsql.iam_authentication`, and enabling it requires an
instance restart the operator declined. `database.auth: password` is the
supported path on this project until that restart happens.

Recorded here in full, with the commands and their output, because a result
that says "no" is still a result and closes this entry.

```
$ gcloud sql instances describe ci-cloudsql --format='value(settings.databaseFlags)'
                                       <- empty: no cloudsql.iam_authentication flag
$ gcloud sql instances list
NAME         DATABASE_VERSION  LOCATION       STATUS
ci-cloudsql  POSTGRES_15       us-central1-a  RUNNABLE
```

The 403s that blocked this in August are gone; both APIs answer. The
`gcloud sql users create --type=cloud_iam_service_account` step was reached and
returned an error naming the missing instance flag, which is what makes this a
measurement rather than an inference.

This fixture is deliberately longer than the evaluator's minimum-length floor.
A discharged section in the real file will be at least this long, because a
result worth recording carries its commands and their output with it.

#### 7.3 The manual smoke test
