# 0011 — Destinations in one line, and making the remote account

Status: accepted
Date: 2026-08-31

## Context

Adding a destination meant a form: type, host, port, user, directory,
repository path, and a password. An operator already knows where the
backups go, and says it in one line — `cpbackup@backup.example.com:/backups`.
Everything in that form except the password is in that line.

The line also assumed the account on the far side already existed. Making
it is the part nobody wants to do: create the user, give it a home, make
the directory, install a key, get the permissions right so sshd will
actually accept it, and remember to lock the password afterwards.

## Decision

`destination.ParseTarget` reads the shapes an operator writes:

    cpbackup@backup.example.com:/backups
    cpbackup@backup.example.com:2222:/srv/backups
    sftp://cpbackup@backup.example.com:2222/srv/backups
    https://user:password@backup.example.com
    s3://bucket/folder
    /mnt/nas/backups

It refuses what it cannot read rather than guessing. A backup written
somewhere other than where it was meant to go is not a backup, and the
operator is right there to be asked. Plain `http://` is refused outright,
and so is a destination at `/`.

Given an administrator's password instead of the backup account's, cprest
creates the account: `useradd`, a locked password so its key is the only
way in, `~/.ssh` and the backup directory at mode 700 owned by the
account, and the public key appended. Then it logs in as that account with
the key and proves it works before saving anything.

Two properties the script is written for, and tested for:

- **Every value is quoted.** It runs as root on a machine that is not
  ours, built from strings an operator typed.
- **Running it twice changes nothing the second time.** An operator who
  retries after a typo should not end up with two half-made accounts.

The administrator's password is used for that one connection and is never
written down — the same rule the account password already followed.

## What this does not do

It does not install rest-server, and it does not configure `--append-only`
on the far side. Both are worth doing and neither is a file operation:
they change what is running on another machine. An operator asking for a
backup account should not get a service they did not ask for.

## Verified

The parser and the script are covered by tests, including the quoting and
the second run. The account-creating path has not been run against a real
remote server here: it needs an administrator's password on the backup
host, which is not something this program's author should be holding.
