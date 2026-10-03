import os, sys, modal
import modal._utils.blob_utils as bu
def boom(url): raise RuntimeError("use_md5 called for " + url)
bu.use_md5 = boom
os.chdir(os.environ["WORK"])
app = modal.App.lookup("cap", create_if_missing=True)
img = modal.Image.debian_slim().add_local_dir("ctx/sub", "/s", copy=True)
img.build(app); print("OK", img.object_id)
