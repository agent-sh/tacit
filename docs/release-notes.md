Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release takes the owner's answer where they give it: in the session they are in. An answer they type there, to any open question, is their answer, and an agent relays it with `tacit questions close --proof words`. Before, some kinds of question took only a mark in tacit app (#111).

- **An agent relays the owner's own answer to any open question.** Code finds the owner's words in their turns, as for every words proof: exact, not a question of theirs, not negated, their newest saying. The words are the owner's answer, and the agent only passes it on, when:
  - they said them on or after the day the question was asked;
  - they said them after the owner's own latest word on the question or its facts: a mark, a reopen, a keep in another answer, a note, This is mine, or an edit, retire, restore or undo of theirs in tacit app. Older words never undo a newer answer of theirs, and the same turn may answer two questions on one fact;
  - the item of their turn that holds the words is on the question's subject (below).
- **What a relayed answer may do.** It answers questions no agent may close on its own proof: a name two files hold, a finding the owner marked, reopened or left a note on, one whose fact they kept in another answer, and an ask addressed to them. It may change their ruling, on their own later words on it. A question of theirs that went moot closes on their answer as `moot`, and nothing changes. While a relayed answer stands, agents' own marks and closures leave the facts it kept alone, and the verifier does not take findings on them. The owner's reopen ends that. Without the owner's words every guard stands as before.
- **A name two files hold takes the owner's answer**: `tacit questions close ID --proof words --answer keep:<path>|both|neither`. Neither is new for a name: it retires both files, each by its path, and the owner's page offers it too. An agent's answer on a name that the store no longer says waits for an answer again.
- **The subject is read from the item of the turn that holds the words.** An answer is often a short line after a dash under the question's own words, which the owner pasted. The line alone shares few words with anything; the words above it carry the subject.
  - The item is the sentences or lines that hold the words, plus the answer's question: the sentences before the answer on its line, or, for an answer line, the line right above it.
  - So the question's words count, and another item's never do.
  - Words that are a question's alone, words that run into the next question, and words in a turn that does not say where its items end (`q; - a; q; - a`) are no answer.
  - At least one shared word must come from the question's facts (their names, descriptions, repository or machine), not the words every question of a kind holds, and not folder names.
  - The same item serves the existing rule on retiring a fact that holds the owner's words.
- **The record says it was relayed.** A relayed answer is a line of `review/decisions.jsonl` like every closure, `by: agent`, with `relayed: true` and the owner's words, where they are, the day and the time (`words_at`). It endorses nothing and makes no ruling. The page shows it as the owner's: "Your answer, relayed by an agent", with their words, the day and the kind of session, and Change this answer undoes it. `tacit questions` says which questions only the owner's own answer closes (`words_only`, `owner_answers` in its JSON).
- **Reopen leaves what it does not change.** Change this answer on a Neither, for a pair or a name, puts back what the answer retired. A fact retired again since on another day keeps that retirement, and a file the reopen does not change is left as it is.
- Known limits, each closed to a relay:
  - The words are at least three words and 18 characters, so a bare yes is quoted with the question's words before it.
  - Text the corpus leaves out of a turn (a paste the prompt history recorded, relayed agent lines) is not in the item.
  - An answer typed without the question's words names no subject.
  - An answer line that ends in "?", a plain answer among dashed ones, an answer that goes on to the next line, a dash that joins two clauses after a bracket or a full stop, and the same words under two items are misread.

Upgrading: no index, store format or protocol change; the server needs no deploy. Install it on the machine that writes as the curator before an agent relays an answer there. An older client refuses a name two files hold in `tacit questions close` and keeps every guard on the owner's questions. It reads a relayed answer as an agent's closure, offers no Neither is true on a name, and cannot reopen a name answered neither; this release can.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
