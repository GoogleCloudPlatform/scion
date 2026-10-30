# Federated project access item 5B: merge forward onto item 5A (765d15b18)

Date: 2026-10-17. Fork PR ptone/scion#4930, tracked in ptone/scion#4603.

- Merged item 5A at 765d15b18 (which already contains upstream main 3fc3b040e) into item 5B at 68a998edd; merging upstream main 3fc3b040e afterwards was a no-op. Two text conflicts, `pkg/hub/authzop/pending.go` and `docs-site/src/content/docs/reference/api.md`, were resolved by keeping item 5A's text and adding item 5B's lines; the diff from 765d15b18 to the merged head equals item 5B's own diff (b54ed9726..68a998edd) apart from the one visitors line in the API reference, which also keeps the upstream wording about service account and proxy driver principals.
