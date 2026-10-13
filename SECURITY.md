# Security

## Reporting a vulnerability

Report it privately through GitHub: the repository's **Security** tab, then **Report a vulnerability**. Do not open a public issue for it.

Include what you found, how to reproduce it, and what an attacker gains. A fix ships before the details are public.

## In scope

- The server: authentication (tailnet identity, signed tokens, key enrollment), the pre-receive scope and secret checks, the public path, the change feed.
- The client: the credential helper, key storage, redaction before a write or a model call, the verifier's read-only probe tools, the local review page and app (loopback, token).
- Anything that lets a writer key write outside its inbox, a reader key push, or a fact reach the store with a secret in it.

## Out of scope

- A compromised tailnet or a compromised machine that holds an enrolled key.
- What a model does with the facts it is given.
- Secrets that scrump's patterns do not recognize. Those are a scrump issue: report them at https://github.com/avifenesh/scrump.

## Design

The security model is in the README ("Security model") and SPEC.md ("Public path", "Rulings").
