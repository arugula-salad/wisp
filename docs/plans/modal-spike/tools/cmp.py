import sys
from google.protobuf import descriptor_pb2
import modal_proto.api_pb2 as a, modal_proto.task_command_router_pb2 as t
import modal
def norm(p):
    p.ClearField("source_code_info")
    for m in p.message_type: strip(m)
    return p
def strip(m):
    for f in m.field: f.ClearField("json_name")
    for n in m.nested_type: strip(n)
s = descriptor_pb2.FileDescriptorSet(); s.ParseFromString(open(sys.argv[1],"rb").read())
byname = {f.name: norm(f) for f in s.file}
for mod in (a,t):
    p = descriptor_pb2.FileDescriptorProto(); mod.DESCRIPTOR.CopyToProto(p); norm(p)
    q = byname[p.name]
    same = p.SerializeToString(deterministic=True) == q.SerializeToString(deterministic=True)
    nm = len(p.message_type); ns = sum(len(x.method) for x in p.service)
    print(modal.__version__, p.name, "IDENTICAL" if same else "DIFFERS", f"msgs={nm} methods={ns}")
    if not same:
        import difflib
        d = list(difflib.unified_diff(str(p).splitlines(), str(q).splitlines(), lineterm="", n=1))
        print("\n".join(d[:40]))
