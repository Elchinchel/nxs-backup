_[Logo picture]_

# Quick reference

---

- **Maintained by**:  
  [Nixys LLC](https://nixys.io)

- **Where to get help:**  
  the [@nxs_backup_chat](https://t.me/nxs_backup_chat), [Maintainer Telegram](https://t.me/r_andreev),
  or [GitHub Issues](https://github.com/nixys/nxs-backup/issues)

- **Where to file issues:**  
  https://github.com/nixys/nxs-backup/issues

# Supported tags and respective `Dockerfile` links

---

- [`v3.0.2`, `latest`](https://github.com/nixys/nxs-backup/blob/main/.docker/Dockerfile-bin)

# What is nxs-backup?

---

nxs-backup is a tool for creating and delivery backus, rotating it locally and on remote storages.

# How to use this image

---

This image contains only a binary file and can be used to create the image you will use.

## Example

Here's a sample use case, creating an image to back up only PostgreSQL.

```dockerfile
FROM nixyslab/nxs-backup-bin AS bin

FROM debian:12-slim

RUN apt update \
    && apt install -yq postgresql-client \
    && apt clean

COPY --from=bin /nxs-backup /usr/local/bin/nxs-backup

CMD nxs-backup start
```

# License

---

[nxs-backup](https://github.com/nixys/nxs-backup) is open source and released under the terms of
the [GNU GPL-3.0 license](https://github.com/nixys/nxs-backup/blob/main/LICENSE).

As for any pre-built image usage, it is the image user's responsibility to ensure that any use of this image complies
with any relevant licenses for all software contained within.