---
title: Licensing
description: AGPL-3.0 server, Apache-2.0 agent, DCO contributions, and why Everwas is a greenfield project rather than a fork.
---

Everwas uses two licenses on purpose:

- **The server is AGPL-3.0.** Anyone can self-host it, modify it, and
  offer it as a service, and improvements to it stay open, including
  improvements made by someone running it as a SaaS. For infrastructure
  you point at your whole fleet, "the operator can read the source of
  what is actually running" is a security property, not just a
  philosophical one.
- **The agent is Apache-2.0.** The agent gets installed on machines you
  may not fully control the licensing environment of, gets embedded in
  images, and gets redistributed by MSPs to their clients. A permissive
  license removes every question a lawyer might raise about putting the
  binary on a customer's endpoint.

Contributions are accepted under the
[Developer Certificate of Origin](https://developercertificate.org/):
sign off your commits, no CLA, no copyright assignment. Nobody acquires
the special right to take the community's work proprietary later.

## Why not a fork

The obvious starting point would have been an existing open-source RMM,
and the most capable candidate changed its license in 2022 to a
source-available, non-commercial license. Code derived from it can never
be open source again, which ruled it out entirely: a derivative would
inherit the restriction forever.

That episode also shaped the licensing above. The failure mode it
demonstrates (a community builds on a project, then the door closes) is
exactly what the AGPL-plus-DCO combination prevents: the license keeps
the code open, and the absence of a CLA means no single party ever holds
the relicensing pen.
