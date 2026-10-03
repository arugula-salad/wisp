"""Checks that every type in the pruned .proto files (compiled to a descriptor
set at argv[1]) is field-for-field the installed modal client's: same names,
numbers, types, labels and oneofs, enums with the same values."""

import sys

from google.protobuf import descriptor_pb2

import modal_proto.api_pb2 as api_pb2
import modal_proto.task_command_router_pb2 as tcr_pb2

orig = {}
for mod in (api_pb2, tcr_pb2):
    p = descriptor_pb2.FileDescriptorProto()
    mod.DESCRIPTOR.CopyToProto(p)
    orig[p.name] = p


def key(d):
    if isinstance(d, descriptor_pb2.EnumDescriptorProto):
        return ("enum", [(v.name, v.number) for v in d.value])
    fields = sorted((f.name, f.number, f.type, f.label, f.type_name, f.proto3_optional,
                     d.oneof_decl[f.oneof_index].name if f.HasField("oneof_index") else "") for f in d.field)
    return ("msg", fields, [(n.name, key(n)) for n in d.nested_type], [(e.name, key(e)) for e in d.enum_type])


s = descriptor_pb2.FileDescriptorSet()
s.ParseFromString(open(sys.argv[1], "rb").read())
n = 0
for p in s.file:
    if p.name not in orig:
        continue
    o = orig[p.name]
    byname = {d.name: d for d in list(o.message_type) + list(o.enum_type)}
    for d in list(p.message_type) + list(p.enum_type):
        if key(d) != key(byname[d.name]):
            sys.exit(f"{p.package}.{d.name} differs from the client's")
        n += 1
    osvc = {sv.name: {m.name: (m.input_type, m.output_type, m.client_streaming, m.server_streaming) for m in sv.method} for sv in o.service}
    for sv in p.service:
        for m in sv.method:
            if osvc[sv.name][m.name] != (m.input_type, m.output_type, m.client_streaming, m.server_streaming):
                sys.exit(f"{sv.name}.{m.name} differs from the client's")
            n += 1
print(f"ok: {n} types and methods match the client's", file=sys.stderr)
