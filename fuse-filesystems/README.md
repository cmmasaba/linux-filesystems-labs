## Setting Up Fuse

1. Install the linux packages:

```bash

sudo apt install libfuse-dev libfuse3-dev gcc
```

2. Reload the gopls language server in VS Code

3. Optionally add these lines to .vscode/settings.json to build using fuse3

```json
{
  "go.buildFlags": ["-tags=fuse3"],
  "gopls": {
    "build.buildFlags": ["-tags=fuse3"]
  }
}
```
