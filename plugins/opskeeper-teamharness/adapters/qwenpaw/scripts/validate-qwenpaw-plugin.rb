#!/usr/bin/env ruby
# frozen_string_literal: true
# validate-qwenpaw-plugin.rb — Validate opskeeper-teamharness QwenPaw zip structure.

require "json"
require "open3"
require "pathname"
require "tmpdir"
require "yaml"

abort("usage: validate-qwenpaw-plugin.rb <generated-plugin-dir>") if ARGV.empty?

package_dir = Pathname.new(ARGV[0]).expand_path

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

def assert_file(path); fail!("missing file: #{path}") unless path.file?; end
def assert_dir(path); fail!("missing directory: #{path}") unless path.directory?; end

def py_compile(path)
  Dir.mktmpdir("opskeeper-pycache-") do |cache_dir|
    Open3.capture3(
      { "PYTHONPYCACHEPREFIX" => cache_dir },
      "python3", "-m", "py_compile", path.to_s,
    )
  end
end

assert_dir(package_dir)
plugin_json = package_dir / "plugin.json"
plugin_py = package_dir / "plugin.py"
task_trace_py = package_dir / "task_trace.py"
asset_dir = package_dir / "opskeeper-teamharness"

assert_file(plugin_json)
assert_file(plugin_py)
assert_file(task_trace_py)
assert_dir(asset_dir)

manifest = JSON.parse(plugin_json.read)
fail!("plugin id must be opskeeper-teamharness") unless manifest["id"] == "opskeeper-teamharness"
fail!("plugin type must be general") unless manifest["type"] == "general"
fail!("backend entry must be plugin.py") unless manifest.dig("entry", "backend") == "plugin.py"

features = manifest.dig("meta", "features") || []
fail!("qwenpaw plugin must not declare periodic-sync") if features.include?("periodic-sync")

assert_file(asset_dir / "plugin.yaml")
source_manifest = YAML.load_file(asset_dir / "plugin.yaml")
version = source_manifest.fetch("metadata").fetch("version")
fail!("version mismatch: plugin.json=#{manifest['version']} plugin.yaml=#{version}") unless manifest["version"] == version

py_compile(plugin_py)
py_compile(task_trace_py)
py_compile(asset_dir / "mcp" / "server.py")
py_compile(asset_dir / "mcp" / "auth.py")

puts "ok: opskeeper-teamharness qwenpaw package #{version} validated"
