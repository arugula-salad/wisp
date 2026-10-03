import os, sys, modal, traceback
LOG = os.environ["CAPLOG"]
def mark(s):
    with open(LOG, "a") as f: f.write(f"\n########## {s}\n")
os.chdir(os.environ["WORK"]); sys.path.insert(0, ".")
app = modal.App.lookup("cap", create_if_missing=True)
I = modal.Image
sec = modal.Secret.from_dict({"TOK": "x"})
cases = {
 "debian_slim": lambda: I.debian_slim(),
 "debian_slim_312": lambda: I.debian_slim(python_version="3.12"),
 "debian_slim_3.11.9": lambda: I.debian_slim(python_version="3.11.9"),
 "micromamba": lambda: I.micromamba(python_version="3.11"),
 "micromamba_install": lambda: I.micromamba("3.11").micromamba_install("numpy", channels=["conda-forge"]),
 "from_registry": lambda: I.from_registry("ubuntu:24.04"),
 "from_registry_add_python": lambda: I.from_registry("ubuntu:24.04", add_python="3.12", setup_dockerfile_commands=["RUN echo setup"]),
 "from_registry_secret": lambda: I.from_registry("ghcr.io/me/priv:1", secret=modal.Secret.from_dict({"REGISTRY_USERNAME":"u","REGISTRY_PASSWORD":"p"})),
 "from_aws_ecr": lambda: I.from_aws_ecr("123.dkr.ecr.us-east-1.amazonaws.com/x:1", secret=sec),
 "from_gcp": lambda: I.from_gcp_artifact_registry("us-docker.pkg.dev/p/r/x:1", secret=sec),
 "from_scratch": lambda: I.from_scratch(),
 "pip_install": lambda: I.debian_slim().pip_install("requests", "numpy==2.0", index_url="https://pypi.org/simple", pre=True),
 "uv_pip_install": lambda: I.debian_slim().uv_pip_install("requests", requirements=["requirements.txt"]),
 "pip_install_from_requirements": lambda: I.debian_slim().pip_install_from_requirements("requirements.txt"),
 "pip_install_from_pyproject": lambda: I.debian_slim().pip_install_from_pyproject("pyproject.toml", optional_dependencies=["dev"]),
 "poetry_install_from_file": lambda: I.debian_slim().poetry_install_from_file("pyproject.toml"),
 "uv_sync": lambda: I.debian_slim().uv_sync("uvproj"),
 "apt_install": lambda: I.debian_slim().apt_install("git", "curl"),
 "run_commands_env_secret_gpu": lambda: I.debian_slim().run_commands("echo a", ["echo b"], env={"A": "1"}, secrets=[sec], gpu="T4"),
 "env_workdir_entry_cmd_shell": lambda: I.debian_slim().env({"X": "a b"}).workdir("/w").entrypoint(["/bin/sh","-c"]).cmd(["sleep","9"]).shell(["/bin/bash","-c"]),
 "dockerfile_commands_ctx": lambda: I.debian_slim().dockerfile_commands(["ARG V=1", "COPY sub/a.txt /a.txt", "RUN cat /a.txt"], context_dir="ctx", build_args={"V": "2"}),
 "dockerfile_commands_context_files": lambda: I.debian_slim().dockerfile_commands("COPY /x.json /x.json", context_files={"/x.json": "data.json"}),
 "from_dockerfile": lambda: I.from_dockerfile("ctx/Dockerfile", context_dir="ctx", build_args={"FOO": "baz"}),
 "from_dockerfile_add_python": lambda: I.from_dockerfile("ctx/Dockerfile", context_dir="ctx", add_python="3.11"),
 "add_local_file_copy": lambda: I.debian_slim().add_local_file("data.json", "/d/data.json", copy=True),
 "add_local_dir_copy_bigfile": lambda: I.debian_slim().add_local_dir("ctx/sub", "/s", copy=True),
 "add_local_python_source_copy": lambda: I.debian_slim().add_local_python_source("mypkg", copy=True),
 "add_local_file_runtime_then_build": lambda: I.debian_slim().add_local_file("data.json", "/d/data.json"),
 "pip_force_build": lambda: I.debian_slim().pip_install("six", force_build=True),
 "build_fail": lambda: I.debian_slim().run_commands("exit 1"),
}
only = sys.argv[1:] or list(cases)
for name in only:
    mark(name)
    try:
        img = cases[name]()
        img.build(app)
        mark(f"{name} -> OK {img.object_id} mount_layers={[m.object_id for m in img._mount_layers]}")
    except Exception as e:
        mark(f"{name} -> EXC {type(e).__name__}: {str(e)[:300]}")
# from_id / publish / from_name / run_function
mark("from_id")
try:
    im = I.from_id("im-abc"); mark(f"from_id hydrated? {im.object_id}")
    sb = None
except Exception as e: mark(f"EXC {e}")
mark("publish_from_name")
try:
    img = I.debian_slim(); img.build(app); img.publish("myimg:v1")
    n = I.from_name("myimg:v1"); 
    try:
        modal.Sandbox.create("true", app=app, image=n)
    except Exception as e: mark(f"sandbox create EXC {type(e).__name__} {e}")
except Exception as e: mark(f"EXC {type(e).__name__}: {e}")
mark("sandbox_with_runtime_mount")
try:
    img = I.debian_slim().add_local_file("data.json", "/d/data.json").add_local_python_source("mypkg")
    modal.Sandbox.create("true", app=app, image=img)
except Exception as e: mark(f"EXC {type(e).__name__}: {e}")
mark("done")
