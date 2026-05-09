---
title: Shell Completions
---

Scion provides shell completions for `bash`, `zsh`, `fish`, and `powershell`.

### Zsh

If you have installed `scion` and want to enable completions:

1.  Generate the completion script:
    ```bash
    scion completion zsh > _scion
    ```
2.  Move the file to a directory in your `$fpath`.

**For macOS users:**
If you are using Homebrew, you likely already have a configured site-functions directory. If you do **not** use Homebrew or prefer a manual setup:

1.  Create the directory if it doesn't exist:
    ```bash
    sudo mkdir -p /usr/local/share/zsh/site-functions
    ```
2.  Move the completion file:
    ```bash
    sudo mv _scion /usr/local/share/zsh/site-functions/
    ```
3.  Ensure that directory is in your `$fpath` in your `~/.zshrc` (usually added automatically, but verify if completions don't work):
    ```bash
    # in ~/.zshrc
    fpath=(/usr/local/share/zsh/site-functions $fpath)
    autoload -U compinit; compinit
    ```

### Bash

To load completions for the current session:
```bash
source <(scion completion bash)
```

To load completions for each session, execute once:
```bash
# Linux:
scion completion bash | sudo tee /etc/bash_completion.d/scion

# macOS:
scion completion bash | sudo tee /usr/local/etc/bash_completion.d/scion
```
