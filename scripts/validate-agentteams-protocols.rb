#!/usr/bin/env ruby
# frozen_string_literal: true

require "yaml"

ROOT = File.expand_path("..", __dir__)
PLUGIN = File.join(ROOT, "plugins/opskeeper-teamharness/plugin.yaml")

document = YAML.safe_load(File.read(PLUGIN), aliases: true)
metadata = document.fetch("metadata")
raise "unexpected plugin name" unless metadata.fetch("name") == "opskeeper-teamharness"
raise "plugin version is missing" if metadata.fetch("version").to_s.strip.empty?

expected_skills = %w[
  opskeeper-alerter
  opskeeper-investigator
  opskeeper-critic
  opskeeper-reviewer
  opskeeper-repairer
  opskeeper-verifier
  opskeeper-postmortem
]
skills = document.fetch("skills").fetch("agent").map { |entry| entry.fetch("id") }
raise "worker skills do not match public contract: #{skills.inspect}" unless skills == expected_skills

skills.each do |skill_id|
  entry = document.fetch("skills").fetch("agent").find { |candidate| candidate.fetch("id") == skill_id }
  path = File.join(ROOT, "plugins/opskeeper-teamharness", entry.fetch("path"))
  raise "missing skill directory for #{skill_id}" unless File.directory?(path)
  raise "missing SKILL.md for #{skill_id}" unless File.file?(File.join(path, "SKILL.md"))
end

expected_tools = %w[
  loop.investigate
  loop.correlate
  recovery.verify
  recovery.execute
  metric.query
  incident.list
  incident.get
  postgres.analyze_status
  host.get_load
  host.get_processes
  host.restart_service
  knowledge.query
  knowledge.write
  hitl.decide
  state.put
  state.get
  incident.record
]
server = document.fetch("mcp").fetch("servers").find { |candidate| candidate.fetch("id") == "opskeeper" }
raise "OpsKeeper MCP server is missing" if server.nil?
tools = server.fetch("tools")
missing_tools = expected_tools - tools
extra_tools = tools - expected_tools
raise "MCP tools are missing: #{missing_tools.inspect}" unless missing_tools.empty?
raise "unexpected MCP tools: #{extra_tools.inspect}" unless extra_tools.empty?

adapters = document.fetch("adapters", []).map { |entry| entry.fetch("id") }
raise "QwenPaw adapter is missing" unless adapters.include?("qwenpaw")

required_tests = %w[
  adapters/qwenpaw/test_readonly_enforcement.py
  mcp/test_alignment.py
  safety/test_levels.py
]
required_tests.each do |relative|
  path = File.join(ROOT, "plugins/opskeeper-teamharness", relative)
  raise "required plugin test is missing: #{relative}" unless File.file?(path)
end

puts "AgentTeams public plugin contract: PASS"
puts "MCP tools: #{tools.join(', ')}"
