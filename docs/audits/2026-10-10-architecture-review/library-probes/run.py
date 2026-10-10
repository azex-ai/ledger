from pathlib import Path
import subprocess
import tempfile

here = Path(__file__).resolve().parent
repo = here.parents[3]
with tempfile.TemporaryDirectory(prefix="library-review-", dir=repo / ".hive-tmp") as tmp:
    go_probe = Path(tmp) / "probe.go"
    go_probe.write_text((here / "probe.go.txt").read_text())
    commands = [["node", str(here / "probe.mjs")], ["go", "run", str(go_probe)]]
    logs = []
    for cmd in commands:
        result = subprocess.run(cmd, cwd=repo, text=True, capture_output=True, timeout=120)
        logs.append("$ " + " ".join(cmd) + "\n" + result.stdout + result.stderr + "\nexit=" + str(result.returncode))
        if result.returncode:
            print("\n".join(logs))
            raise SystemExit(result.returncode)
    output = "\n".join(logs) + "\n"
    (here / "output.txt").write_text(output)
    print(output)
