# sockt (Python)

Install from this folder:

```bash
pip install -e .
```

Usage:

```python
from sockt import ComputeClient

client = ComputeClient(api_url="https://api.sockt.dev", api_key="sockt_live_...")
sb = client.create_sandbox(billing_method="credits", template="sockt-default")
print(sb.pod_id, sb.data_endpoint)
out = sb.exec("uname -a")
print(out.stdout)
```

Environment variables are documented in `../.env.example`.
