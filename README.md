# sciget

Install and run containerised scientific software, on a workstation or an HPC
login node. sciget reads a catalogue of published containers, fetches each
container's image, and runs it through Apptainer. One static binary, no root.

> **Status: under rebuild, not yet released.** The commands below work today;
> running and installing containers are still to come.

## Building

```sh
go build
go test ./...
```

## Commands

| Command | What it does |
|---|---|
| `sciget sync` | Download the latest catalogue |
| `sciget find [words...]` | List the containers in the catalogue (`--all` includes hidden ones) |
| `sciget config [get\|set\|unset\|list]` | Show and change settings |
| `sciget completion <shell>` | Print a shell completion script |
| `sciget version` | Print the version |

## Licence

MIT. See [LICENSE](LICENSE).
