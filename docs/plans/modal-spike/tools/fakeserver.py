# Recording fake ModalClient server (grpclib) for capturing what modal 1.6.0 sends for images.
import asyncio, hashlib, json, os, sys, time, base64, itertools
import grpclib.const, grpclib.server
from grpclib.server import Server
from google.protobuf import text_format, empty_pb2
from modal_proto import api_pb2, api_grpc
from aiohttp import web

LOG = open(os.environ.get("CAPLOG", "cap.log"), "a")
def log(name, msg):
    LOG.write(f"=== {name}\n{text_format.MessageToString(msg, as_utf8=True)[:6000]}\n"); LOG.flush()

BLOBS = {}; FILES = {}; IMAGES = {}; MOUNTS = {}; TAGS = {}
HTTP_PORT = int(os.environ.get("HTTP_PORT", "18999"))
ctr = itertools.count()
FAIL_SUBSTR = os.environ.get("FAIL_SUBSTR", "exit 1")

def img_id(img, bv):
    h = hashlib.sha256(img.SerializeToString(deterministic=True) + bv.encode()).hexdigest()
    return "im-" + h[:22]

class H(api_grpc.ModalClientBase):
    pass

async def unary(name, stream, fn):
    req = await stream.recv_message()
    log(name, req)
    resp = await fn(req)
    await stream.send_message(resp)

def _jwt():
    import base64, json, time
    e=lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
    return e({"alg":"none"})+"."+e({"exp": int(time.time())+3600})+".x"
async def AuthTokenGet(req): return api_pb2.AuthTokenGetResponse(token=_jwt())
async def ClientHello(req): return api_pb2.ClientHelloResponse()
async def AppGetOrCreate(req): return api_pb2.AppGetOrCreateResponse(app_id="ap-x")
async def AppCreate(req): return api_pb2.AppCreateResponse(app_id="ap-x")
async def EnvironmentGetOrCreate(req):
    return api_pb2.EnvironmentGetOrCreateResponse(environment_id="en-x", metadata=api_pb2.EnvironmentMetadata(name="main", settings=api_pb2.EnvironmentSettings(image_builder_version="2025.06")))
async def ImageGetOrCreate(req):
    i = img_id(req.image, req.builder_version)
    fail = any(FAIL_SUBSTR in c for c in req.image.dockerfile_commands)
    IMAGES[i] = (req, fail)
    # never answer built: force ImageJoinStreaming
    return api_pb2.ImageGetOrCreateResponse(image_id=i)
async def MountGetOrCreate(req):
    m = "mo-" + hashlib.sha256(req.SerializeToString(deterministic=True)).hexdigest()[:22]
    return api_pb2.MountGetOrCreateResponse(mount_id=m, handle_metadata=api_pb2.MountHandleMetadata(content_checksum_sha256_hex="00"))
async def MountBatchedCheckExistence(req):
    return api_pb2.MountBatchedCheckExistenceResponse(missing_sha256_hex_hashes=[h for h in req.sha256_hex_hashes if h not in FILES])
async def MountPutFile(req):
    if req.WhichOneof("data_oneof"):
        FILES[req.sha256_hex] = True
        return api_pb2.MountPutFileResponse(exists=True)
    return api_pb2.MountPutFileResponse(exists=req.sha256_hex in FILES)
async def BlobCreate(req):
    b = f"bl-{next(ctr)}"
    if os.environ.get("MULTIPART"):
        n = max(1, -(-req.content_length // (2<<20)))
        mp = api_pb2.MultiPartUpload(part_length=2<<20, upload_urls=[f"http://127.0.0.1:{HTTP_PORT}/blob/{b}/{i}" for i in range(n)], completion_url=f"http://127.0.0.1:{HTTP_PORT}/complete/{b}")
        return api_pb2.BlobCreateResponse(blob_ids=[b], multiparts=api_pb2.MultiPartUploadList(items=[mp]))
    return api_pb2.BlobCreateResponse(blob_ids=[b], upload_urls=api_pb2.UploadUrlList(items=[f"http://127.0.0.1:{HTTP_PORT}/blob/{b}"]))
async def SecretGetOrCreate(req): return api_pb2.SecretGetOrCreateResponse(secret_id=f"st-{next(ctr)}")
async def ImageFromId(req): return api_pb2.ImageFromIdResponse(image_id=req.image_id, metadata=api_pb2.ImageMetadata(image_builder_version="2025.06"))
async def ImageGetByTag(req): return api_pb2.ImageGetByTagResponse(image_id=TAGS.get(req.tag, "im-nosuch"))
async def ImagePublish(req):
    TAGS[req.tag] = req.image_id; return api_pb2.ImagePublishResponse(image_id=req.image_id, revision_id="rv-1")
async def ImageBuildChainGet(req): return api_pb2.ImageBuildChainGetResponse()
async def AppClientDisconnect(req): return empty_pb2.Empty()
async def AppHeartbeat(req): return empty_pb2.Empty()
async def AppPublish(req): return api_pb2.AppPublishResponse()
async def AppSetObjects(req): return empty_pb2.Empty()
async def VolumeGetOrCreate(req): return api_pb2.VolumeGetOrCreateResponse(volume_id="vo-x")
async def FunctionPrecreate(req): return api_pb2.FunctionPrecreateResponse(function_id="fu-x")
async def FunctionCreate(req): return api_pb2.FunctionCreateResponse(function_id="fu-x")
async def SandboxCreateV2(req): raise grpclib.GRPCError(grpclib.const.Status.UNIMPLEMENTED, "captured")
async def SandboxCreate(req): raise grpclib.GRPCError(grpclib.const.Status.UNIMPLEMENTED, "captured")

async def ImageJoinStreaming(stream):
    req = await stream.recv_message(); log("ImageJoinStreaming", req)
    r, fail = IMAGES[req.image_id]
    for i, c in enumerate(r.image.dockerfile_commands):
        await stream.send_message(api_pb2.ImageJoinStreamingResponse(entry_id=f"{i}-0",
            task_logs=[api_pb2.TaskLogs(data=f"Step {i}: {c}\n", file_descriptor=api_pb2.FILE_DESCRIPTOR_STDOUT)]))
    if fail:
        res = api_pb2.GenericResult(status=api_pb2.GenericResult.GENERIC_STATUS_FAILURE, exception="Build step failed: exit 1")
    else:
        res = api_pb2.GenericResult(status=api_pb2.GenericResult.GENERIC_STATUS_SUCCESS)
    await stream.send_message(api_pb2.ImageJoinStreamingResponse(result=res, eof=True, entry_id="end",
        metadata=api_pb2.ImageMetadata(image_builder_version="2025.06", workdir="/", python_version_info="3.14.2")))

import inspect
UNARY = {k: v for k, v in list(globals().items()) if k[0].isupper() and inspect.iscoroutinefunction(v) and k != "ImageJoinStreaming"}

def mapping(self):
    base = api_grpc.ModalClientBase.__mapping__(self)
    out = {}
    for path, h in base.items():
        name = path.rsplit("/", 1)[1]
        if name == "ImageJoinStreaming":
            fn = ImageJoinStreaming
        elif name in UNARY:
            f = UNARY[name]
            fn = (lambda n, f: (lambda stream: unary(n, stream, f)))(name, f)
        else:
            async def fn(stream, name=name, resp_t=h.reply_type):
                req = await stream.recv_message(); log(name + " (UNHANDLED)", req)
                await stream.send_message(resp_t())
        out[path] = grpclib.const.Handler(fn, h.cardinality, h.request_type, h.reply_type)
    return out
H.__mapping__ = mapping
H.__abstractmethods__ = frozenset()

async def blob_put(request):
    data = await request.read()
    LOG.write(f"=== HTTP PUT {request.path} len={len(data)} headers={dict(request.headers)}\n"); LOG.flush()
    return web.Response(status=200, headers={"ETag": '"%s"' % hashlib.md5(data).hexdigest()})

PARTS = {}
async def blob_put2(request):
    data = await request.read(); PARTS.setdefault(request.match_info["id"], {})[int(request.match_info["n"])] = hashlib.md5(data).digest()
    LOG.write(f"=== HTTP PUT part {request.path} len={len(data)} md5hdr={request.headers.get('Content-MD5')} ctype={request.headers.get('Content-Type')}\n"); LOG.flush()
    return web.Response(status=200, headers={"ETag": '"%s"' % hashlib.md5(data).hexdigest()})
async def complete(request):
    body = await request.text(); parts = PARTS[request.match_info["id"]]
    et = hashlib.md5(b"".join(parts[i] for i in sorted(parts))).hexdigest() + f"-{len(parts)}"
    LOG.write(f"=== HTTP POST complete {request.path}\n{body}\n"); LOG.flush()
    return web.Response(status=200, text=f"<CompleteMultipartUploadResult><ETag>&quot;{et}&quot;</ETag></CompleteMultipartUploadResult>")
async def main():
    app = web.Application(client_max_size=1 << 30); app.router.add_put("/blob/{id}", blob_put); app.router.add_put("/blob/{id}/{n}", blob_put2); app.router.add_post("/complete/{id}", complete)
    runner = web.AppRunner(app); await runner.setup(); await web.TCPSite(runner, "127.0.0.1", HTTP_PORT).start()
    s = Server([H()]); await s.start("127.0.0.1", int(os.environ.get("PORT", "18998")))
    print("listening", flush=True); await s.wait_closed()
asyncio.run(main())
