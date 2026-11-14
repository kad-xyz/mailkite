import json,subprocess,sys
sys.path.insert(0,sys.argv[1]); from mcpbench import Client
c=Client(sys.argv[4:]); c.init()
want=sys.argv[2].split(",")
for t in c.send("tools/list",{})["result"]["tools"]:
    if t["name"] in want:
        p=t.get("inputSchema",{}).get("properties",{})
        print(t["name"],"req=",t.get("inputSchema",{}).get("required"),{k:(v.get("type"),str(v.get("description",""))[:50]) for k,v in p.items()})
c.close()
