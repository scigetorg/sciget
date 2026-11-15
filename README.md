# sciget

Install and run containerised scientific software, on a workstation or an HPC
login node. sciget reads a catalogue of published containers, fetches each
container's image, and runs it through Apptainer. One static binary, no root.

> **Status: not yet released.** sciget runs containers that are already on
> the machine, such as those on a CVMFS share; a container that is not there
> is an error until downloads arrive. Installing containers as desktop
> launchers (`sciget install`) is planned and not yet available.

## Building

```sh
CGO_ENABLED=0 go build   # a static binary
go test ./...
```

## First run

sciget starts with no catalogues. Add one, saying where its containers are,
then download it:

```sh
sciget catalogue add neuroimaging https://apps-json.neurodesk.workers.dev/neuroimaging/apps.json \
    --dir /cvmfs/neurodesk.ardc.edu.au/containers
sciget refresh
sciget
```

Run on a terminal with no command, sciget opens a full-screen interface:
search, pick a container and open a shell in it or launch its applications.
Mouse and keyboard both work.

Catalogue URLs must use https. Plain http is allowed only to this machine,
such as a port forwarded through an SSH tunnel
(`http://localhost:8080/apps.json`). sciget uses the standard `HTTPS_PROXY`,
`HTTP_PROXY` and `NO_PROXY` variables.

## Commands

| Command | What it does |
|---|---|
| `sciget` or `sciget ui` | Browse and launch containers in a full-screen interface |
| `sciget catalogue [add\|remove\|use\|unuse\|list]` | Manage catalogues; of those in use, the first listing a name wins |
| `sciget refresh` | Download the latest catalogues in use |
| `sciget find [words...]` | List the containers in the catalogues in use (`--all` includes hidden ones) |
| `sciget info <tool/version>` | Show a container's details and where it is |
| `sciget config [get\|set\|unset\|list]` | Show and change settings |
| `sciget completion <shell>` | Print a shell completion script |
| `sciget version` | Print the version |

## Licence

MIT. See [LICENSE](LICENSE).
