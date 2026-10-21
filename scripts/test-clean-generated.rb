#!/usr/bin/env ruby
require_relative 'workspace-paths'
require 'tmpdir'
require 'fileutils'
require_relative 'clean-generated'

Dir.mktmpdir('proletarius-clean-test') do |root|
  derived = File.join(root, 'build/optimization/DerivedData-fixture')
  protected = File.join(root, 'build/optimization/source-snapshot')
  outside = File.join(root, 'outside')
  [derived + '/Build/Products', derived + '/Build/Intermediates.noindex', protected, outside].each do |path|
    FileUtils.mkdir_p(path)
  end
  File.write(derived + '/info.plist', 'fixture')
  File.write(derived + '/Build/Products/app', 'application')
  File.write(derived + '/Build/Intermediates.noindex/cache', 'generated')
  File.write(protected + '/source.tar.gz', 'irreplaceable source')
  File.write(outside + '/keep', 'external')
  File.symlink(outside, derived + '/ModuleCache.noindex')
  File.symlink(outside, derived + '/Build/Intermediates.noindex/foreign')
  cleaner = GeneratedCacheCleaner.new(root)
  expected = 'build/optimization/DerivedData-fixture/Build/Intermediates.noindex'
  raise 'unsafe candidate list' unless cleaner.candidates == [expected]
  raise 'dry-run removed files' unless File.read(protected + '/source.tar.gz') == 'irreplaceable source'
  ['build', 'build/optimization', 'rust/target', '../outside', expected + '/../Products'].each do |invalid|
    rejected = false
    begin
      cleaner.remove!([invalid])
    rescue RuntimeError
      rejected = true
    end
    raise "accepted unsafe path: #{invalid}" unless rejected
  end
  cleaner.remove!([expected])
  raise 'cache not removed' if File.exist?(File.join(root, expected))
  raise 'backup changed' unless File.read(protected + '/source.tar.gz') == 'irreplaceable source'
  raise 'product changed' unless File.read(derived + '/Build/Products/app') == 'application'
  raise 'followed nested symlink' unless File.read(outside + '/keep') == 'external'
  # An entire apparently valid DerivedData root reached through a symlink is excluded.
  File.symlink(derived, File.join(root, 'build/LinkedBuild'))
  raise 'followed root symlink' unless cleaner.candidates.empty?
end
puts 'PASS: default dry-run, exact allowlist, snapshots/products preserved, symlink isolation'
