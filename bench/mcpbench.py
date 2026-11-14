#!/usr/bin/env python3
"""Minimal stdio MCP client for benchmarking Apple Mail MCP servers.

usage:
  mcpbench.py list  -- CMD...            # tools count + schema size
  mcpbench.py call  SPEC.json -- CMD...  # timed calls, SPEC = [{"name":..,"args":{..},"repeat":N}]
Prints JSON. Process RSS/CPU are sampled via ps while it runs.
"""
import json, subprocess, sys, time, threading


class Client:
    def __init__(self, cmd):
        self.t0 = time.perf_counter()
        self.p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.id = 0
        self.peak_rss = 0
        self._stop = False
        threading.Thread(target=self._sample, daemon=True).start()

    def _sample(self):
        while not self._stop and self.p.poll() is None:
            try:
                out = subprocess.run(["ps", "-o", "rss=", "-p", str(self.p.pid)],
                                     capture_output=True, text=True).stdout.strip()
                if out:
                    self.peak_rss = max(self.peak_rss, int(out))
            except Exception:
                pass
            time.sleep(0.05)

    def send(self, method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        if not notify:
            self.id += 1
            msg["id"] = self.id
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()
        if notify:
            return None
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError("server closed stdout")
            try:
                r = json.loads(line)
            except ValueError:
                continue
            if r.get("id") == self.id:
                return r

    def init(self):
        r = self.send("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                     "clientInfo": {"name": "bench", "version": "0"}})
        self.send("notifications/initialized", notify=True)
        self.startup = time.perf_counter() - self.t0
        return r

    def cpu_seconds(self):
        out = subprocess.run(["ps", "-o", "time=", "-p", str(self.p.pid)],
                             capture_output=True, text=True).stdout.strip()
        # formats: M:SS.ss or H:MM:SS
        parts = [float(x) for x in out.replace(",", ".").split(":")] if out else [0]
        s = 0.0
        for x in parts:
            s = s * 60 + x
        return s

    def close(self):
        self._stop = True
        try:
            self.p.stdin.close()
            self.p.wait(timeout=5)
        except Exception:
            self.p.kill()


def main():
    mode = sys.argv[1]
    sep = sys.argv.index("--")
    cmd = sys.argv[sep + 1:]
    c = Client(cmd)
    c.init()
    res = {"startup_s": round(c.startup, 3)}
    tools = c.send("tools/list", {})["result"]["tools"]
    blob = json.dumps(tools, ensure_ascii=False)
    res.update(tools=len(tools), schema_chars=len(blob), schema_tokens_est=len(blob) // 4)
    if mode == "list":
        res["names"] = [t["name"] for t in tools]
    else:
        spec = json.load(open(sys.argv[2]))
        res["calls"] = []
        for s in spec:
            times, err, size = [], None, 0
            for _ in range(s.get("repeat", 3)):
                t = time.perf_counter()
                r = c.send("tools/call", {"name": s["name"], "arguments": s.get("args", {})})
                times.append(time.perf_counter() - t)
                if "error" in r or r.get("result", {}).get("isError"):
                    err = json.dumps(r.get("error") or r["result"].get("content"), ensure_ascii=False)[:300]
                size = len(json.dumps(r.get("result", {}), ensure_ascii=False))
            times.sort()
            res["calls"].append({"tool": s["name"], "label": s.get("label", s["name"]),
                                 "median_ms": round(times[len(times) // 2] * 1000, 1),
                                 "min_ms": round(times[0] * 1000, 1), "resp_chars": size, "error": err})
        # idle CPU: measure cpu time over 5 s of doing nothing
        c0 = c.cpu_seconds(); time.sleep(5); res["idle_cpu_5s"] = round(c.cpu_seconds() - c0, 3)
    res["cpu_total_s"] = round(c.cpu_seconds(), 3)
    res["peak_rss_mb"] = round(c.peak_rss / 1024, 1)
    c.close()
    print(json.dumps(res, ensure_ascii=False, indent=1))


if __name__ == "__main__":
    main()
