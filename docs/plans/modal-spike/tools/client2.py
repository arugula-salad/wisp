import os, sys, modal
LOG = os.environ["CAPLOG"]
def mark(s):
    with open(LOG, "a") as f: f.write(f"\n########## {s}\n")
os.chdir(os.environ["WORK"]); sys.path.insert(0, ".")
app = modal.App.lookup("cap", create_if_missing=True)
I = modal.Image
for name, mk in [("from_id", lambda: I.from_id("im-415db68125d688348842c5")),
                 ("runtime_mounts", lambda: I.debian_slim().add_local_file("data.json", "/d/data.json").add_local_python_source("mypkg")),
                 ("from_name", lambda: I.from_name("myimg:v1"))]:
    mark(name)
    try:
        modal.Sandbox.create("true", app=app, image=mk())
    except Exception as e: mark(f"EXC {type(e).__name__}: {e}")
mark("layer_on_from_id")
try:
    I.from_id("im-415db68125d688348842c5").pip_install("six").build(app)
except Exception as e: mark(f"EXC {type(e).__name__}: {e}")
mark("done")
