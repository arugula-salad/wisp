# The spike's target, verbatim (docs/providers/modal.md): it must print hi.
import modal
app = modal.App.lookup("wisp-spike", create_if_missing=True)
sb = modal.Sandbox.create("sleep", "infinity", app=app, image=modal.Image.debian_slim())
p = sb.exec("echo", "hi"); print(p.stdout.read())
sb.terminate()
