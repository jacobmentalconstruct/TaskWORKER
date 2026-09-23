"""Role / system prompt / prompt inference in the shared, running service.

    python examples/basic_infer.py --model qwen2.5:0.5b "Say hello in five words."

The command is written to disk BEFORE it is sent. If anything below reports an
uncertain create, run recover_uncertain.py with the same file.
"""

import json
import sys

from _common import parser, utf8_stdout

from taskworker_client import Client, JobEnded, RequestTimeout, ServiceFault, TaskWorkerError, submit_command


def main():
    utf8_stdout()
    p = parser(__doc__)
    p.add_argument("prompt")
    p.add_argument("--model", required=True, help="exact installed model id (see: taskworker models)")
    p.add_argument("--role", default="Be concise.")
    p.add_argument("--system", default="Use plain text.")
    p.add_argument("--max-output-tokens", type=int, default=64)
    p.add_argument("--command-file", default="command.json", help="where the exact command is saved before sending")
    p.add_argument("--wait-timeout", type=float, default=120.0)
    args = p.parse_args()

    client = Client(args.server)
    command = submit_command(model=args.model, role=args.role, system_prompt=args.system, prompt=args.prompt,
                             max_output_tokens=args.max_output_tokens, origin="python-example")
    command.save(args.command_file)
    print(f"saved {args.command_file}; key={command.key}", file=sys.stderr)
    try:
        job = client.infer(command, wait_timeout=args.wait_timeout)
    except JobEnded as ended:  # observed failure / cancellation / interruption
        print(f"job {ended.job.id} ended {ended.job.state}; retained partial output:", file=sys.stderr)
        print(ended.partial_text)
        return 4
    except RequestTimeout as exc:
        print(f"timeout (acceptance {'uncertain' if exc.acceptance_uncertain else 'not proven'}): {exc}", file=sys.stderr)
        print(f"recover with: python recover_uncertain.py {args.command_file}", file=sys.stderr)
        return 1
    except ServiceFault as fault:
        print(f"service fault {fault.code} (retryable={fault.retryable}): {fault.message}", file=sys.stderr)
        return 1
    except TaskWorkerError as exc:
        print(f"{type(exc).__name__}: {exc}", file=sys.stderr)
        return 1
    print(job.text)
    print(json.dumps({"id": job.id, "state": job.state, "usage": job.get("result", {}).get("usage")}), file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
