"""Writes the subset of the modal client's protocol that frontend/modal serves.

The modal wheel ships no .proto files, only the generated *_pb2.py modules,
which embed the serialized FileDescriptorProtos. This script reads those from
an installed modal (pinned: gen.sh installs it), keeps the RPCs listed below
and every message and enum they reach, and prints them back as .proto source
with the original package names, type names and field numbers, so the wire
format is exactly the client's. Run it through gen.sh.
"""

import sys

from google.protobuf import descriptor_pb2

import modal
import modal_proto.api_pb2 as api_pb2
import modal_proto.task_command_router_pb2 as tcr_pb2

KEEP = {
    "modal.client.ModalClient": [
        "AppGetOrCreate",
        "EnvironmentGetOrCreate",
        "ImageGetOrCreate",
        "ImageJoinStreaming",
        "AuthTokenGet",
        "SandboxCreateV2",
        "SandboxGetTaskIdV2",
        "SandboxGetCommandRouterAccess",
        "SandboxWaitV2",
        "SandboxTerminateV2",
        # The V1 sandbox path (MODAL_SANDBOX_V2=0, and sb- IDs): the same
        # handlers under other names.
        "SandboxCreate",
        "SandboxGetTaskId",
        "TaskGetCommandRouterAccess",
        "SandboxWait",
        "SandboxTerminate",
    ],
    "modal.task_command_router.TaskCommandRouter": [
        "TaskExecStart",
        "TaskExecStdioRead",
        "TaskExecWait",
        "TaskExecPoll",
    ],
}

SCALARS = {
    1: "double", 2: "float", 3: "int64", 4: "uint64", 5: "int32", 6: "fixed64", 7: "fixed32",
    8: "bool", 9: "string", 12: "bytes", 13: "uint32", 15: "sfixed32", 16: "sfixed64",
    17: "sint32", 18: "sint64",
}

files = {}
for mod in (api_pb2, tcr_pb2):
    p = descriptor_pb2.FileDescriptorProto()
    mod.DESCRIPTOR.CopyToProto(p)
    files[p.name] = p

# Every message and enum by full name ("modal.client.Foo.Bar"), with the
# top-level message it lives in: keeping a type keeps its whole top-level
# declaration, nested types and all.
types = {}  # full name -> (file name, top-level full name, descriptor)


def index(fname, prefix, top, msgs, enums):
    for e in enums:
        types[prefix + e.name] = (fname, top or prefix + e.name, e)
    for m in msgs:
        full = prefix + m.name
        types[full] = (fname, top or full, m)
        index(fname, full + ".", top or full, m.nested_type, m.enum_type)


for fname, p in files.items():
    index(fname, p.package + ".", None, p.message_type, p.enum_type)

kept = set()  # top-level full names


def keep(name):
    name = name.lstrip(".")
    if name.startswith("google.protobuf."):
        return
    top = types[name][1]
    if top in kept:
        return
    kept.add(top)
    walk(types[top][2])


def walk(d):
    if isinstance(d, descriptor_pb2.EnumDescriptorProto):
        return
    for f in d.field:
        if f.type_name:
            keep(f.type_name)
    for n in d.nested_type:
        walk(n)


services = {}
for fname, p in files.items():
    for s in p.service:
        full = p.package + "." + s.name
        if full not in KEEP:
            continue
        methods = {m.name: m for m in s.method}
        missing = [n for n in KEEP[full] if n not in methods]
        if missing:
            sys.exit(f"{full} has no {missing} in modal {modal.__version__}")
        services[full] = (fname, s.name, [methods[n] for n in KEEP[full]])
        for m in services[full][2]:
            keep(m.input_type)
            keep(m.output_type)


def tname(t, pkg):
    t = t.lstrip(".")
    return t[len(pkg) + 1:] if t.startswith(pkg + ".") else "." + t


def emit_enum(e, ind, out):
    out.append(f"{ind}enum {e.name} {{")
    if e.options.allow_alias:
        out.append(f"{ind}  option allow_alias = true;")
    for v in e.value:
        out.append(f"{ind}  {v.name} = {v.number};")
    for r in e.reserved_range:
        out.append(f"{ind}  reserved {r.start} to {r.end};" if r.end != r.start else f"{ind}  reserved {r.start};")
    out.append(f"{ind}}}")


def ftype(f, pkg, maps):
    if f.type in (11, 14):
        return tname(f.type_name, pkg)
    return SCALARS[f.type]


def emit_msg(m, ind, pkg, out):
    out.append(f"{ind}message {m.name} {{")
    maps = {n.name: n for n in m.nested_type if n.options.map_entry}
    for e in m.enum_type:
        emit_enum(e, ind + "  ", out)
    for n in m.nested_type:
        if not n.options.map_entry:
            emit_msg(n, ind + "  ", pkg, out)
    oneofs = {}
    for f in m.field:
        if f.HasField("oneof_index") and not f.proto3_optional:
            oneofs.setdefault(f.oneof_index, []).append(f)
    done = set()
    for f in m.field:
        if f.HasField("oneof_index") and not f.proto3_optional:
            i = f.oneof_index
            if i in done:
                continue
            done.add(i)
            out.append(f"{ind}  oneof {m.oneof_decl[i].name} {{")
            for g in oneofs[i]:
                out.append(f"{ind}    {ftype(g, pkg, maps)} {g.name} = {g.number};")
            out.append(f"{ind}  }}")
            continue
        entry = maps.get(f.type_name.rsplit(".", 1)[-1]) if f.type == 11 and f.label == 3 else None
        if entry is not None:
            k, v = entry.field
            out.append(f"{ind}  map<{ftype(k, pkg, {})}, {ftype(v, pkg, {})}> {f.name} = {f.number};")
            continue
        label = "repeated " if f.label == 3 else ("optional " if f.proto3_optional else "")
        out.append(f"{ind}  {label}{ftype(f, pkg, maps)} {f.name} = {f.number};")
    for r in m.reserved_range:
        out.append(f"{ind}  reserved {r.start} to {r.end - 1};" if r.end - 1 != r.start else f"{ind}  reserved {r.start};")
    out.append(f"{ind}}}")


def uses_wkt(d, out):
    if isinstance(d, descriptor_pb2.EnumDescriptorProto):
        return
    for f in d.field:
        if f.type_name.startswith(".google.protobuf."):
            out.add("google/protobuf/" + f.type_name.split(".")[-1].lower() + ".proto")
    for n in d.nested_type:
        uses_wkt(n, out)


outdir = sys.argv[1]
for fname, p in files.items():
    out = [
        f"// Generated by prune.py from the modal {modal.__version__} wheel's {fname}",
        "// (Apache-2.0, Copyright Modal Labs; see LICENSE in this directory).",
        "// A subset: only what frontend/modal serves. DO NOT EDIT.",
        "",
        'syntax = "proto3";',
        "",
        f"package {p.package};",
        "",
    ]
    tops = [d for d in list(p.enum_type) + list(p.message_type) if p.package + "." + d.name in kept]
    wkt = set()
    for d in tops:
        uses_wkt(d, wkt)
    deps = set()
    for d in tops:
        def deps_of(d):
            if isinstance(d, descriptor_pb2.EnumDescriptorProto):
                return
            for f in d.field:
                if f.type_name and not f.type_name.startswith(".google."):
                    fn = types[f.type_name.lstrip(".")][0]
                    if fn != fname:
                        deps.add(fn)
            for n in d.nested_type:
                deps_of(n)
        deps_of(d)
    for svc in services.values():
        if svc[0] == fname:
            for m in svc[2]:
                for t in (m.input_type, m.output_type):
                    fn = types[t.lstrip(".")][0]
                    if fn != fname:
                        deps.add(fn)
    for imp in sorted(wkt) + sorted("modal_proto/" + d.split("/")[-1] for d in deps):
        out.append(f'import "{imp}";')
    out += ["", 'option go_package = "github.com/arugula-salad/wisp/frontend/modal/modalpb";', ""]
    for d in tops:
        if isinstance(d, descriptor_pb2.EnumDescriptorProto):
            emit_enum(d, "", out)
        else:
            emit_msg(d, "", p.package, out)
        out.append("")
    for full, (sf, sname, methods) in services.items():
        if sf != fname:
            continue
        out.append(f"service {sname} {{")
        for m in methods:
            cs = "stream " if m.client_streaming else ""
            ss = "stream " if m.server_streaming else ""
            out.append(f"  rpc {m.name}({cs}{tname(m.input_type, p.package)}) returns ({ss}{tname(m.output_type, p.package)});")
        out.append("}")
        out.append("")
    with open(f"{outdir}/{fname.split('/')[-1]}", "w") as fh:
        fh.write("\n".join(out))
    print(f"{fname}: {len(tops)} top-level types", file=sys.stderr)
