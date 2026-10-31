# tacit

## Personal content

The repository is public. Personal content never enters it:

- the owner's words, quoted from chats, transcripts or their store, dated or not;
- measurements of their store, sessions, spend, history or machines: counts, scores, costs, sizes, load;
- their host names, IP addresses, tailnet names and machine layout;
- names of their private projects and repositories, and fact names or contents from their store;
- home paths that reveal any of these.

Test fixtures are synthetic. Design text states the rule a remark motivated, not the remark, and the mechanism a measurement showed, not its numbers ("measured on a real store; the numbers stay with the owner"). Measurements, transcripts and other evidence stay on the machine that made them, outside the repository. Not personal: the author's name and email as git author, the public names agent-sh/* and avifenesh/scrump, generic examples, and tacit's own settings and latencies.

`python3 scripts/privacy-lint.py` checks the shapes this content came in as: words attributed to the owner with a date, tailnet addresses and names, home directories of real users, and the words of a private deny list of host and project names. The list stays out of the repository: the lint reads `--deny FILE`, the `TACIT_PRIVACY_DENY` variable and tacit/privacy-deny.txt in the XDG config directory, all that are there, and CI reads a secret. CI runs it on every pull request, every push to main and once a day. Run it before you push.

## Overview

Memory for coding agents in Markdown and git: a Go client (`client/`), an Elixir server (`server/`) and the eval scripts (`eval/`). SPEC.md is the contract: a change in behavior changes SPEC.md in the same change.
