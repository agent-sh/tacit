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
