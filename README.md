# sciget

Install and run containerised scientific software, on a workstation or an HPC
login node. sciget reads a catalogue of published containers, fetches each
container's image, and runs it through Apptainer. One static binary, no root.

> **Status: not yet released.** sciget runs containers on a CVMFS share or
> downloaded with `sciget pull` from Nectar or S3. Downloads from the
> Quay and GHCR registries, and modules, are planned.

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
| `sciget pull <tool/version>...` | Download containers into `container_dir`, `parallel_pulls` (3) at a time |
| `sciget doctor` | Report problems, such as stopped downloads, and the commands that fix them |
| `sciget config [get\|set\|unset\|list]` | Show and change settings |
| `sciget completion <shell>` | Print a shell completion script |
| `sciget version` | Print the version |

## Pulling containers

`sciget pull fsl/6.0.7.8` downloads a container that is not already on
the machine (on its catalogue's share, or downloaded before) into
`container_dir` (default `~/.local/share/sciget/containers`). It tries each of `download_urls` in turn, Nectar then S3
by default. A download counts only when its size matches what the server
reported, its SHA-256 matches the catalogue's (when the catalogue gives
one), and Apptainer can open it. Set `warn_missing_checksum` to true to be
told when no SHA-256 was published. Before downloading, sciget checks the
disk has room (quotas are not seen).

Running a container that is not on the machine (`sciget apptainer exec`,
`shell`, or a launch in the interface) asks first, since the cause may be
a share that is not mounted: `[y]es, [n]o, [A]lways (yes, saved as the
default)`. Always sets `auto_pull`, after which sciget downloads without
asking. Outside a terminal, with `auto_pull` off, it stops and says how
to get the container. In the interface, downloads show in Activity with a
Cancel button; quitting while one runs asks first, then stops it.

A stopped download starts again from the beginning; a killed one leaves a
`*.incomplete` file, which `sciget doctor` lists. sciget does not delete
containers: to remove one, delete its folder in `container_dir`.

Keep `container_dir` separate from neurocommand's container folder: an
image sciget puts there makes neurocommand skip setting it up.

## Licence

MIT. See [LICENSE](LICENSE).
