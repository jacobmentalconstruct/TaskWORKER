"""Observe a job live, then optionally cancel it explicitly.

    python examples/observe_and_cancel.py --model qwen2.5:0.5b --cancel-after 3 "Count slowly to 200."

Closing the stream (or pressing Ctrl+C) detaches this observer only; the job
keeps running. Only client.cancel() stops inference, and the output produced
before the backend stopped is retained.
"""

import sys
import threading

from _common import parser, utf8_stdout

from taskworker_client import Client, CursorError, Mirror, ServiceFault, StreamClosed, submit_command


def main():
    utf8_stdout()
    p = parser(__doc__)
    p.add_argument("prompt")
    p.add_argument("--model", required=True)
    p.add_argument("--max-output-tokens", type=int, default=400)
    p.add_argument("--cancel-after", type=float, help="seconds before an explicit cancel")
    args = p.parse_args()

    client = Client(args.server)
    mirror = Mirror(client.snapshot())        # atomic view + cursor
    job = client.send(submit_command(model=args.model, prompt=args.prompt,
                                     max_output_tokens=args.max_output_tokens, origin="python-example"))
    print(f"job {job.id} accepted as {job.state}", file=sys.stderr)

    timer = None
    if args.cancel_after:
        # Cancellation is a separate, explicit request; it could come from any client.
        def cancel():
            print(f"\n-- cancelling {job.id}", file=sys.stderr)
            client.cancel(job.id)

        timer = threading.Timer(args.cancel_after, cancel)
        timer.daemon = True
        timer.start()

    printed = 0
    try:
        # follow() resumes from mirror.cursor and, with max_reconnects, reconnects
        # from the last APPLIED cursor a bounded number of times.
        for frame, applied in client.follow(mirror, max_reconnects=2):
            if not applied or job.id not in mirror.jobs:
                continue
            text = mirror.text(job.id)
            if len(text) > printed:
                print(text[printed:], end="", flush=True)
                printed = len(text)
            if mirror.job(job.id).is_terminal:
                break
    except KeyboardInterrupt:
        print(f"\ndetached; job {job.id} keeps running (cancel it with client.cancel)", file=sys.stderr)
        return 130
    except (StreamClosed, CursorError, ServiceFault) as exc:
        # Never treated as completion; resume from mirror.cursor or take a new snapshot.
        print(f"\nobservation stopped: {exc}; last applied cursor {mirror.cursor}", file=sys.stderr)
        return 1
    finally:
        if timer:
            timer.cancel()
    final = client.result(job.id)
    print(f"\n-- {final.state}; {len(final.text.encode('utf-8'))} bytes retained", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
