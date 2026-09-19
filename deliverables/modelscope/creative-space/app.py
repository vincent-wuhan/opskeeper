from pathlib import Path

import gradio as gr

ROOT = Path(__file__).resolve().parent
CONTENT = next(
    path for path in (ROOT / "content" / "zh.md", ROOT / "zh.md") if path.exists()
).read_text(encoding="utf-8")
ASSET_CANDIDATES = [
    ROOT / "assets" / "evidence",
    ROOT / "evidence",
    ROOT,
    ROOT.parent / "assets" / "evidence",
]
ASSETS = next(path for path in ASSET_CANDIDATES if (path / "monitor.png").exists())

EXTERNAL_LINKS = """
<div class="entry-grid">
  <a href="https://github.com/vincent-wuhan/opskeeper" target="_blank" rel="noopener noreferrer">GitHub 源码</a>
  <a href="https://opskeeper.yueming.xin" target="_blank" rel="noopener noreferrer">产品官网</a>
  <a href="https://opskeeper.yueming.xin" target="_blank" rel="noopener noreferrer">OpsKeeper 服务</a>
  <a href="https://rooms.yueming.xin" target="_blank" rel="noopener noreferrer">AgentTeams Rooms</a>
  <a href="https://teams.yueming.xin" target="_blank" rel="noopener noreferrer">AgentTeams Dashboard</a>
  <a href="https://opskeeper.yueming.xin/live-incident" target="_blank" rel="noopener noreferrer">路演全流程控制台</a>
</div>
"""

css = """
.entry-grid {display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:12px;margin:16px 0}
.entry-grid a {display:block;padding:14px;border:1px solid #d9dde6;border-radius:12px;color:#1652f0;text-decoration:none;font-weight:600}
.entry-grid a:hover {border-color:#1652f0;background:#f4f7ff}
"""

with gr.Blocks(title="OpsKeeper · Agent 原生运维工作台", theme=gr.themes.Base(), css=css) as demo:
    gr.Markdown(CONTENT)
    gr.HTML(EXTERNAL_LINKS)
    with gr.Tabs():
        with gr.Tab("总览"):
            gr.Image(str(ASSETS / "opskeeper-overview.gif"), label="OpsKeeper overview", show_label=False)
        with gr.Tab("监控与根因"):
            gr.Gallery([str(ASSETS / "monitor.png"), str(ASSETS / "rca-session.png")], label="Monitoring and RCA", columns=2)
        with gr.Tab("编排与拓扑"):
            gr.Gallery([str(ASSETS / "workflow-editor.png"), str(ASSETS / "topology-map.png")], label="Workflow and topology", columns=2)
        with gr.Tab("审批与档案"):
            gr.Gallery([str(ASSETS / "write-gate.png"), str(ASSETS / "artifacts.png"), str(ASSETS / "knowledge-vault.png")], label="Approval and archive", columns=2)

if __name__ == "__main__":
    demo.launch(server_name="0.0.0.0", server_port=7860, show_api=False)
