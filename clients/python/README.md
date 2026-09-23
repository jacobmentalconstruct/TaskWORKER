# taskworker-client

Standard-library-only Python client for a **running** TaskWorker service
(`taskworker serve`). It never starts a service, opens the journal or runs
inference. Supported Python: 3.10 through 3.14 (CPython, all platforms).

Install from this directory (`pip install ./clients/python`), or use it without
installing by putting `clients/python/src` on `PYTHONPATH` (or copying the
`taskworker_client` folder next to your script). Full documentation:
[docs/python.md](../../docs/python.md).

```python
from taskworker_client import Client, submit_command

client = Client()                      # TASKWORKER_SERVER or http://127.0.0.1:7433
command = submit_command(model="qwen2.5:0.5b", role="Be concise.",
                         system_prompt="Use plain text.", prompt="Say hello.",
                         max_output_tokens=64)
command.save("command.json")           # keep it BEFORE sending; needed to recover
job = client.infer(command)            # one create, then observe; never resubmits
print(job.text)
```
