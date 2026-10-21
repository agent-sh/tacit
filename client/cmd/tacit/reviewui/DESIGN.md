# Proof, plain: the design language of tacit app and tacit review

The owner opens tacit to answer two questions: what do I need to decide, and what is true. Every screen answers one of them in plain words. The owner never needs to know how tacit works inside to use it: no proofs, galleys, slugs, stets, deles, relations, supersedes, kinds of finding or frontmatter on screen. Those words stay in the code and in SPEC.md.

The identity is the one tacit review started with (Proof: Literata to read, Atkinson to operate, ink on stock, one crimson pencil), set calmer: fewer sizes, fewer rules, more space, and the pencil only on the action that matters most.

## Words

| On screen | What it is |
|---|---|
| Fact | A curated fact agents can be shown |
| Note | An inbox note an agent wrote, not reviewed yet |
| Your rule | A ruling: their own words, provably (`fact.IsRuling`: `source.kind: user` with a quote of theirs, their decision, or written by them) |
| What agents believe you prefer | A `user_standard` an agent filed that is not their ruling, or a fact that holds a few words of theirs (`source.owner_words`) |
| Your words in it | The phrase of theirs a fact that is not their rule holds (`source.owner_words`) |
| In your words | The phrase of theirs a ruling rests on (`source.owner_quote`) |
| This is mine | The owner makes such a fact their rule (a `mine` decision, `source.endorsed`) |
| Retire, Restore | Set or clear `retired` |
| Keep the newer, Keep the older | The pair marks, named by what they keep |
| Who it reaches | The scope, said as "in memra", "on workstation", "about you, in every repo", "for every agent, everywhere" |
| What agents read | The description |
| Only when working on | `applies_to` (files, code names, repos, machines, commands) |
| Details | Name, file, kind, source, links, history, git |

Sentence case. A button says what will happen ("Keep the newer"), the slip says what happened ("Kept the newer fact."), the record of an answer uses the same words ("You answered: Kept the newer fact, 9 Oct, 06:11."). Errors say what failed and what to do next. Dates read "3 Oct" (the year only when it is not this year); times are 24-hour. No em dashes.

## Places

Five places in one top bar, the current one in ink with an underline: Needs you (keys 1), Your rules (2), What agents believe you prefer (3), All facts (4), Inbox (5). New rule (n) and Dark or Light (d) sit at the right. Nothing else is chrome.

## Needs you: one decision

```
 9 left                                              Previous  Next  Filter
 These two facts disagree. Which is true now?
 An agent suggests: keep the newer. Its reason: ...

  Older, 14 Sep  Your rule                Newer, 20 Sep
  what the older fact says                what the newer fact says
  In memra                                In memra
  Details                                 Details

  [Keep the newer ->] [Keep the older <-] [Both are true s] [Mark as conflicting q] [Skip for now j]
  Keeps the newer fact. The older fact stops reaching agents; it stays in history.

 What git shows
  In memra, 60+ commits since 14 Sep changed what these facts name, the latest on 22 Sep: "...".
  Full git history
```

- One finding at a time. "9 left" is the only count. Previous and Next (k, j) walk the queue; Skip for now (j) moves on and saves nothing.
- The question is plain per kind (below). Older and newer always follow the dates the cards print, for every kind: where the curator's roles run the other way, the question says so ("Does the older fact replace the newer one?"). The agent's question is never a headline. What it would do is one sentence and the matching choice is the primary button, also when it named no fact (a stale check on a rule: "An agent suggests: outdated, retire it"; a conflict on a rule: "mark as conflicting"); a question in its own words reads "An agent asks: ...". In its reason only a standalone A or B that refers to a fact becomes "the older fact" or "your rule", and only a date that stands alone becomes "14 Sep"; fact names and English stay as written.
- Each fact is a card: who it is (Older, Newer, Your rule, Saved number, Number from git) with the day it speaks for (the newest date in its text, else the day it was written), what it says (description, then body; a long body folds to five lines), where it applies, and Details (name, kind, dates, file). The older card is always on the left; when the data has it on the right the cards swap and so do the arrow keys, so the left arrow keeps the left card.
- Choices are a few bordered buttons, icon then label then key (keys in semibold ink-2, arrows a step larger). The choice the agent suggests leads and is the one pencil button; with no suggestion none is. The line under the buttons says what the leading choice does before anything is pointed at, and what the pointed one does after; when their rule is the fact that goes it says "Your rule stops reaching agents". Pointing writes on the cards what happens to each ("Would stop reaching agents", "Stays", "Marked as conflicting"); label mode writes nothing on the cards, and its hints say the store does not change.
- A changed filter starts at the first finding.
- What git shows is a few sentences built on the server from the evidence (`Timeline.Summary`): what the facts name that is gone, per repo how much changed since and the latest change, which repos were quiet or unreadable. The dated ledger of commits is folded under Full git history.
- A stale check sets the card on the left and what git shows on the right: what was deleted, where, when and in which commit, read from the finding's own lines.
- Every pair also offers Neither is true (w, an x): both facts stop reaching agents, both cards grey on hover, and Change this answer brings both back exactly.
- Under the choices, Add a note (optional): their words, sent with whatever the owner answers, or alone with Comment without answering (a speech bubble), which leaves the finding open. Their notes show on the finding as Your notes; an answered one shows "Your note:" under "You answered".
- Filter (/) folds out find, the kinds that have something open, the scope, and Needs you, Past answers or Everything. An answered finding shows "You answered: ..." with Change this answer (z).
- When nothing is left: "Nothing needs you." and See past answers.

| Kind | Question | Choices |
|---|---|---|
| contradiction | These two facts disagree. Which is true now? | Keep the newer, Keep the older (first, second when the dates match), Both are true, Mark as conflicting, Neither is true |
| supersede-proposed | Does the newer fact replace the older one? (by the printed dates; "the older fact replace the newer one" when the roles run the other way) | Keep the newer, Keep the older, Both are true, Mark as conflicting |
| result-superseded | A newer number exists for this measurement. Use it? | Use the number from git, Keep the saved number, Both are true, Mark as conflicting |
| ruling-change | A newer fact may change your rule. Does your rule still stand? | Keep your rule, Use the newer fact, Both are true, Mark as conflicting |
| supersede-applied | The curator replaced the older fact with the newer one. Keep it that way? | Keep it replaced, Undo, both are true, Keep the older instead, Mark as conflicting |
| duplicate | These two facts say the same thing. Keep one? | Keep the newer, Keep the older, Both are true, Mark as conflicting |
| stale | Git says something this fact names was deleted. Is it still true? (a rule: Does your rule still hold?) | Still true, Outdated, retire it |
| link | Your rule links to a fact that does not exist. | Apply the fix, Edit the rule |

Every pair kind adds Neither is true. Keys: the arrows keep that card, s both are true (still true on a stale check), q mark as conflicting, w neither is true, r outdated, a apply the fix, e edit the rule, z change the answer or undo the last one, j k next and previous, / filter. Label mode (tacit review --label) records Neither is true as a label instead, and adds Not sure, skip (x) and Only I can know (o).

## Your rules, What agents believe you prefer, All facts, Inbox: search first

A list on the left (420 px), the fact on the right. The list head says what the place is in one line ("Rules you set. Agents read them as rules in force, and no agent may change or retire them."; "Notes agents wrote that the curator has not reviewed yet. Agents are not shown them until they are."), then the search field and one Filter control that folds out status, facts or inbox notes, who it reaches, repo, kind and source, with Clear the filter. A row is the fact's description (two lines at most) over where it applies, its date, and "your rule" or "retired". A rule that rests on their words shows them under the description in italic ink-2: In your words: “...”. What agents believe you prefer says in its head line that these are the agents' words, not theirs.

A fact opens with what it says: the description large (a step smaller when it runs past 140 characters), the body under it (when the description is only the body's start, the body folds under Read the whole note), then one sentence of where it applies and when it was written or changed, then, for their rule, one italic line of why it is theirs (In your words: “...”, You said this is yours, You wrote this), then its actions: Edit (e) in pencil, Retire or Restore (r), Undo last change (u, only when there is one), or Make it my rule (p) for a note. A curated fact that is not their rule also offers This is mine (m), in pencil when the fact is one agents believe the owner prefers; a fact agents believe the owner prefers also offers Not mine (x); a fact the owner said is not theirs is tagged You said not yours, says so in its italic line with their recorded words, if any, and offers Undo Not mine (x) in place of Not mine. A fact that holds a few words of theirs, too few to make it their rule, says them in one italic line (Your words in it: “...”) and that no agent retires it against them without asking them. Nothing of this is asked in Needs you: the place is browsed when the owner wants. Details folds the name, file, kind, source, who it reaches, what it is limited to, links both ways, the changes to this fact in the store ("you" for theirs) and What git shows, read only when Details opens.

Edit is one column: What agents read (with its count of 200), More detail, Who it reaches as the same plain choices New rule offers (with "Or a scope" for any other), Kind, Only when working on folded unless it limits more than the scope says, then Save (ctrl Enter) in pencil and Cancel (Esc). A draft over a fact that changed in the store puts In the store now beside it, its changed lines underlined in the query blue, with Keep my draft (m) in pencil, Take the new version (t) and Cancel; focus starts on Keep my draft and nothing is saved until the owner chooses. After Keep my draft the store's version stays beside the form, folded on request, until the draft is saved.

New rule asks "What is the rule?": in your words, which repo it is about, who it reaches narrowest first (each with who that is; user or system adds the line that agents tend to widen their remarks), what agents read (their first sentence unless the owner writes one), and the file name folded.

## Colour

| Token | Light | Dark | Role |
|---|---|---|---|
| `--stock` | `#f5f6f2` | `#15171b` | Page |
| `--galley` | `#fbfbf8` | `#1c1f24` | Cards, filter panel, the current row |
| `--ink` | `#1c1d1a` | `#e7e5de` | Type; where you are; focus |
| `--ink-2` | `#55574f` | `#b0b0a8` | Body text, secondary type |
| `--ink-3` | `#6b6d64` | `#8f9089` | Dates, keys, hints (4.5:1 on stock) |
| `--line` | `#dfe0d9` | `#2b2f35` | The one hairline under the top bar and between sections |
| `--line-2` | `#b9bbb2` | `#4a4e55` | Button borders, field rules |
| `--pencil` | `#b3262c` | `#ff8a7a` | The primary action only; "would stop reaching agents" |
| `--query` | `#2b52b8` | `#94b2ff` | Lines a draft lacks; "marked as conflicting" |

Pencil means "this is the action that matters here": the suggested answer, Edit, Save, Keep my draft, Make it my rule, Apply the fix. Nothing else is crimson. Where you are is ink.

## Type

Literata for what is read (questions, what facts say, git sentences), Atkinson Hyperlegible Next for what is operated (buttons, labels, lists' second lines). Four sizes on a minor third: 13.3 (labels, hints, dates), 16 (body, buttons), 19.2 (what a fact says in a card or a list), 27.6 (the question, a fact's description). Lines stay under 80 characters.

## Icons

One family drawn inline as SVG on a 20 px grid: one 1.6 px stroke in the label's colour, round caps and joins, no fills, no decoration. An icon leads its label and never replaces it. check (keep, still true, save), two checks (both are true), not-equal (mark as conflicting), x (neither is true, cancel), speech bubble (comment without answering), question in a ring (not sure), double chevron (skip), box (retire), box with an up arrow (restore), curved arrow (undo, change the answer), pencil (edit, keep my draft), circling arrows (take the new version), link (apply the fix), plus (new rule, make it my rule), magnifier (search), three lines (filter), chevrons (previous, next), moon and sun (dark, light). Static buttons name their icon in `data-icon` and the page draws it, so there is one source.

## Layout and motion

Needs you is one centred column, 1180 px at most. Generous space: 8 px grid, 32 px between blocks. At most one hairline per section; cards are a step lighter than the page, without borders. Motion only answers them: pointing at a choice greys and strikes the text of the card it would retire. Reduced motion turns transitions off. Below 900 px the places scroll under the brand, cards stack, the list folds above the fact.

## Theme

Dark or Light follows the system until the owner chooses; their choice is kept. When the system scheme changes while the page is open (WebKitGTK may report it late), the page follows and the button repaints to say what it will do.

## Controls in the desktop window

WebKitGTK would draw a select as a GTK menu and radios as GTK widgets. No select is shown: each is a ruled button with a chevron and a list the page draws (arrows, Home, End, Page keys, type-ahead, Enter or Space, Esc). Radios are drawn rings. Search fields are plain text fields (Esc clears). Text fields grow with what they hold, so no resize grip shows, and scrollbars are thin in the line colour. No native popup ever opens.

## Checked

Every screen at 1600 by 1000 (the window) and 1920 by 1080 (a common laptop and half-width desktop size), in both lights, in a browser (`tacit app --browser`) and in the real window.

## Page and server

Styles and script are files beside the page (app.css, app.js, desk.css, desk.js, api.js), never inline, under a strict content policy: same origin only, no framing. tacit review and tacit app --browser serve app.html, the window's page, to the URL that carries the run's token; tacit review --label (index.html) runs app.js and app.css alone: Needs you without the other places. A rotation question ("Checked by the curator") is one fact, drawn as a stale check is, with what the curator found beside the card in place of what git shows.
