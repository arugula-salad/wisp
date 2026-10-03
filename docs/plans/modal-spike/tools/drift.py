import sys
from google.protobuf import descriptor_pb2
def load(fn):
    s=descriptor_pb2.FileDescriptorSet(); s.ParseFromString(open(fn,"rb").read())
    meth={}; msgs={}
    def walk(prefix, m):
        for f in m.field: f.ClearField("json_name")
        msgs[prefix+m.name]=m.SerializeToString(deterministic=True)
    for p in s.file:
        if not p.name.startswith("modal_proto"): continue
        for m in p.message_type: walk(p.package+".", m)
        for e in p.enum_type: msgs[p.package+"."+e.name]=e.SerializeToString(deterministic=True)
        for sv in p.service:
            for m in sv.method: meth[sv.name+"."+m.name]=(m.input_type,m.output_type,m.client_streaming,m.server_streaming)
    return meth,msgs
a=load(sys.argv[1]); b=load(sys.argv[2])
am,bm=a[0],b[0]
added=sorted(set(bm)-set(am)); removed=sorted(set(am)-set(bm))
chg=[k for k in set(am)&set(bm) if am[k]!=bm[k]]
mchg=[k for k in set(a[1])&set(b[1]) if a[1][k]!=b[1][k]]
print(f"{sys.argv[1]} -> {sys.argv[2]}: methods {len(am)}->{len(bm)} +{len(added)} -{len(removed)} sigchanged {len(chg)}; types +{len(set(b[1])-set(a[1]))} -{len(set(a[1])-set(b[1]))} changed {len(mchg)}")
if len(sys.argv)>3:
    print(" added:", added); print(" removed:", removed)
    keep=set(sys.argv[3].split(","))
    print(" changed types touching wisp subset:", sorted(k for k in mchg if any(x in k for x in keep)))
