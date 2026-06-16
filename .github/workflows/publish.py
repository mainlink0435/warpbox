import json, os, glob
from urllib.request import Request, urlopen
from urllib.error import HTTPError

tag = os.environ["TAG"]
token = os.environ["GITHUB_TOKEN"]
repo = os.environ["GITHUB_REPOSITORY"]
api = f"https://api.github.com/repos/{repo}"
headers = {"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json"}

release_id = None
try:
    req = Request(f"{api}/releases/tags/{tag}", headers=headers)
    with urlopen(req) as resp:
        release = json.loads(resp.read())
    release_id = release["id"]
    print(f"Found existing release {release_id}")
except HTTPError as e:
    if e.code == 404:
        print("No existing release found")
    else:
        raise

if release_id:
    req = Request(f"{api}/releases/{release_id}", headers=headers, method="DELETE")
    urlopen(req)
    print(f"Deleted release {release_id}")

with open("release_body.md") as f:
    body = f.read()

payload = json.dumps({"tag_name": tag, "name": tag, "body": body, "draft": False}).encode()
req = Request(f"{api}/releases", data=payload, headers=headers)
with urlopen(req) as resp:
    new_release = json.loads(resp.read())
rid = new_release["id"]
base = new_release["upload_url"].replace("{?name,label}", "")
print(f"Created release {rid}")

for path in glob.glob("dist/*"):
    name = os.path.basename(path)
    fh = dict(headers)
    fh["Content-Type"] = "application/octet-stream"
    url = f"{base}?name={name}"
    with open(path, "rb") as f:
        data = f.read()
    req = Request(url, data=data, headers=fh)
    with urlopen(req) as resp:
        print(f"Uploaded {name} ({len(data)} bytes)")

print("Publish complete")
