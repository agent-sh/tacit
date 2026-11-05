# App screenshots

These are unedited captures of the browser app at 1440 × 900, using the tracked `eval/examples/store` fixture. No operator store or model settings were used. The selected machine fact has its source and git history expanded. The images cover both themes.

To recreate the example in a separate store from the repository root:

```sh
export TACIT_STORE="$HOME/.cache/tacit/screenshot-demo/store"
export TACIT_INDEX="$HOME/.cache/tacit/screenshot-demo/index"
export TACIT_LOCAL="$HOME/.cache/tacit/screenshot-demo/overlay"
tacit init
cp -R eval/examples/store/. "$TACIT_STORE/"
tacit index
tacit app --browser
```

Choose **All facts**, select **Scratch files go under ~/.cache, not /tmp**, then open **Details**. Use the light/dark button to switch themes. The example fixture contains generic project and workstation names.

## Diagram and charts

The README's figures are SVG, one file per theme, in the app's colors, naming its font with system fallbacks (an image cannot load a web font). Each is made by a script, so edit the script and run it again rather than the SVG:

- `how-it-works-{light,dark}.svg`: `python3 -I docs/images/diagram.py`
- `recall-context-*.svg` and `recall-latency-*.svg`, with their data in `recall-tasks.csv`, `recall-summary.csv` and `recall-run.json` (tool versions, seed, launches per call): `python3 -I eval/recall/bench.py` ([Recall benchmark](../../eval/README.md#recall-benchmark)).
