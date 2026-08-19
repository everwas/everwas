# Contributing to Everwas

Thanks for being here. The short version: sign your commits off, run the
tests, and expect licensing to be taken seriously, because it is the
product's spine.

## The DCO, and why there is no CLA

Every commit needs a `Signed-off-by` line (`git commit -s`), which asserts
the [Developer Certificate of Origin](DCO): you wrote the change or have
the right to submit it. You keep your copyright. There is no CLA and never
will be one; nobody, including this project, should ever accumulate the
rights to relicense the commons out from under its users. The full
reasoning lives at
[everwas.supported.systems/licensing](https://everwas.supported.systems/licensing/).

## Licensing of contributions

- Changes under `server/`, `web/`, `docsite/`, `site/`: **AGPL-3.0**
- Changes under `agent/`: **Apache-2.0**

The boundary between the two is the NATS wire contract
(`docs/nats-subjects.md`), and it is enforced by mirrored builders with
matching tests on both sides. If your change touches a subject, change the
contract doc and both builders in the same commit; CI refuses drift.

## Development setup

Requirements: Docker, `uv`, Go 1.22+, Node 20+.

```sh
git clone https://github.com/everwas/everwas && cd everwas
cp .env.example .env
cd server && uv run everwas gen-nats-keys   # paste both values into .env
cd .. && make dev
make migrate
make admin EMAIL=you@example.com
```

Tests:

```sh
cd server && uv run pytest        # needs the dev stack up for the full set
cd agent && go test ./...
```

## What a good change looks like here

- Bitemporal writes go through the bitemporal store, never around it.
  "What did we believe at T" must stay answerable forever.
- An empty result is a claim, not a default. Collectors that failed to
  look must say so rather than reporting an empty set.
- Failures are loud. A feature that silently switches itself off (see the
  CA passphrase note in `docs/adr/0003`) is a bug even when nothing
  crashes.
- Commit messages explain why. The history is the design document.

## Security

Do not open public issues for vulnerabilities. Email the maintainers at
[Supported Systems](https://supported.systems) and expect an answer within
72 hours.
