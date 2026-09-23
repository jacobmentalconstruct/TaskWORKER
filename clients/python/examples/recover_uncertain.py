"""Recover from an uncertain create by resending the IDENTICAL saved command.

    python examples/recover_uncertain.py command.json

A timeout or dropped connection after sending does not tell you whether the
service accepted the command. Never make a new key or a new command: resend the
saved file. The service returns the original job when the key and content match
(even after a restart) and reports `conflict` if the content differs.
"""

import sys

from _common import parser, utf8_stdout

from taskworker_client import Client, Command, ServiceFault, TaskWorkerError


def main():
    utf8_stdout()
    p = parser(__doc__)
    p.add_argument("command_file")
    p.add_argument("--operation", choices=("submit", "retry", "branch"), default="submit")
    args = p.parse_args()
    command = Command.load(args.operation, args.command_file)
    print(f"resending {command!r}", file=sys.stderr)
    try:
        job = Client(args.server).send(command)
    except ServiceFault as fault:
        if fault.code == "conflict":
            print("conflict: this key was used with different content; do NOT invent a new key", file=sys.stderr)
        print(f"{fault.code}: {fault.message}", file=sys.stderr)
        return 1
    except TaskWorkerError as exc:
        print(f"still uncertain ({type(exc).__name__}); resend the same file again later", file=sys.stderr)
        return 1
    print(f"resolved to job {job.id} ({job.state})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
