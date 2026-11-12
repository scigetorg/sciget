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

## First run

sciget starts with no catalogues. Add one, then download it:

```sh
sciget catalogue add neuroimaging https://apps-json.neurodesk.workers.dev/neuroimaging/apps.json
sciget refresh
sciget find fsl
```

## Commands

| Command | What it does |
|---|---|
| `sciget catalogue [add\|remove\|use\|unuse\|list]` | Manage catalogues; of those in use, the first listing a name wins |
| `sciget refresh` | Download the latest catalogues in use |
| `sciget find [words...]` | List the containers in the catalogues in use (`--all` includes hidden ones) |
| `sciget config [get\|set\|unset\|list]` | Show and change settings |
| `sciget completion <shell>` | Print a shell completion script |
| `sciget version` | Print the version |

## Licence

MIT. See [LICENSE](LICENSE).
