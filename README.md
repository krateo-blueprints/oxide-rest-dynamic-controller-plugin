# oxide-rest-dynamic-controller-plugin

A [Krateo](https://krateo.io) `rest-dynamic-controller` **plugin** (wrapper web
service) for the Oxide `OxideInstance` resource.

The generic rest-dynamic-controller only speaks CRUD, but bringing an Oxide
instance up needs two actions that aren't create/get/update/delete — **attach the
boot disk** and **start** — and Oxide's create-time `boot_disk` attach silently
no-ops when the disk isn't provisioned yet. Applying a disk and an instance
together therefore races: the instance lands stopped with a detached boot disk
and never self-heals.

This plugin fixes that by making **GET an idempotent reconcile**: on every
observe it (re)attaches the boot disk recorded in `boot_disk_id` and, once
attached, starts the instance. Because the controller GETs on every resync, the
instance converges to *running* regardless of apply ordering — a small
finite-state machine in front of the Oxide API.

It forwards the caller's `Authorization` bearer token to Oxide and targets the
silo given by `OXIDE_API_URL`. Point the `OxideInstance` RestDefinition's OAS
`servers[0].url` at this service; the paths (`/v1/instances/...`) are unchanged.

## Run

```
OXIDE_API_URL=https://<silo> ./plugin   # listens on :8080
```
