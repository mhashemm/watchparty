"""Runs probe experiments under load, on Linux, macOS and Windows: three busy loops on <core>,
and mpv pinned to it (LOAD), so mpv's threads lose races as on a loaded machine. macOS can't
pin a process to a core, so there a busy loop runs on every core instead. Runs every experiment
and exits 1 if any failed.
Usage: python3 run.py <core> <probe.py|probe2.py|...> <experiment:rounds>...
On 0.40 too: docker run --rm --init --cpuset-cpus=2,3 -v "$PWD":/p -w /p watchparty-e2e
python3 run.py 2 ... (the e2e image)."""
import os, subprocess, sys

HERE = os.path.dirname(os.path.abspath(__file__))
exec(open(os.path.join(HERE, "probe.py")).read().partition("\nclass Pipe")[0])

core, probe = sys.argv[1], sys.argv[2]
mac = sys.platform == "darwin"
busy = []
for _ in range(os.cpu_count() if mac else 3):
    p = subprocess.Popen([sys.executable, "-c", "while 1: pass"])
    if sys.platform == "linux":
        os.sched_setaffinity(p.pid, {int(core)})
    elif not mac:
        pin(p, core)
    busy.append(p)
os.environ["LOAD"] = "" if mac else core
print(subprocess.run([MPV, "--version"], capture_output=True, text=True).stdout.splitlines()[0], flush=True)
status = 0
try:
    for e in sys.argv[3:]:
        name, _, rounds = e.partition(":")
        try:
            if subprocess.run([sys.executable, "-I", os.path.join(HERE, probe), name, rounds], cwd=HERE, timeout=1800).returncode:
                status = 1
        except subprocess.TimeoutExpired:
            print(f"{name}: timed out", flush=True)
            status = 1
finally:
    for p in busy:
        p.kill()
sys.exit(status)
