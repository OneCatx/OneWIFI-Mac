#!/usr/bin/env ruby
# Never classify an entire build directory or untracked source as disposable.
require 'fileutils'
require 'find'
require 'json'
require 'open3'

class GeneratedCacheCleaner
  CACHE_PATHS = %w[Build/Intermediates.noindex CompilationCache.noindex
                   ModuleCache.noindex SDKStatCaches.noindex
                   SDKExplicitPrecompiledModules].freeze

  def initialize(root)
    @root = File.realpath(root)
  end

  def real_directory?(path)
    File.directory?(path) && File.realpath(path) == File.expand_path(path)
  rescue Errno::ENOENT, Errno::ELOOP
    false
  end

  def candidates
    # Only recognised Xcode build roots, never arbitrary source snapshots.
    roots = Dir.glob(File.join(@root, 'build', '*', 'info.plist')) +
            Dir.glob(File.join(@root, 'build', 'optimization', 'DerivedData-*', 'info.plist'))
    paths = roots.flat_map do |info|
      directory = File.dirname(info)
      next [] unless real_directory?(directory) && !File.symlink?(info)
      next [] if File.exist?(File.join(directory, 'source.tar.gz'))
      next [] unless real_directory?(File.join(directory, 'Build'))
      CACHE_PATHS.map { |suffix| File.join(directory, suffix) }
    end
    cargo = File.join(@root, 'build', 'rust-target')
    paths << cargo if File.file?(File.join(cargo, '.rustc_info.json'))
    paths.select { |path| real_directory?(path) }.uniq.sort.map { |path| relative(path) }
  end

  def relative(path)
    path.delete_prefix(@root + '/')
  end

  def allocated_bytes(relative_path)
    bytes = 0
    Find.find(File.join(@root, relative_path)) do |path|
      stat = File.lstat(path)
      bytes += stat.blocks * 512 if stat.file?
    end
    bytes
  end

  def remove!(selected)
    raise '必须指定完整的相对缓存路径；不接受通配符或整个 build/' if selected.empty?
    allowed = candidates
    raise '包含不在缓存白名单内的路径' unless selected.uniq == selected &&
      selected.all? { |path| allowed.include?(path) }
    # A fail-closed guard against deleting a cache used by an active build.
    processes, status = Open3.capture2('/bin/ps', '-axo', 'comm=')
    raise '无法确认当前构建进程，取消清理' unless status.success?
    if processes.lines.any? { |line| %w[xcodebuild cargo rustc swift-frontend compile link].include?(File.basename(line.strip)) }
      raise '检测到构建进程，请等待构建结束后再清理'
    end
    selected.each do |path|
      full = File.join(@root, path)
      raise '缓存路径已发生变化，取消清理' unless real_directory?(full) && candidates.include?(path)
      FileUtils.remove_entry_secure(full)
    end
    selected
  end
end

if $PROGRAM_NAME == __FILE__
  cleaner = GeneratedCacheCleaner.new(File.expand_path('../..', __dir__))
  begin
    if ARGV.empty? || ARGV == ['--list']
      entries = cleaner.candidates.map { |path| {path: path, allocated_bytes: cleaner.allocated_bytes(path)} }
      puts JSON.pretty_generate(mode: 'dry_run', caches: entries,
                                note: '保留源码、快照、恢复材料、日志、Build/Products 和 dist。仅 --remove 精确路径 执行清理。')
    elsif ARGV.shift == '--remove'
      puts JSON.pretty_generate(removed: cleaner.remove!(ARGV),
                                recovery: '仅删除可重新编译生成的缓存；首次重建可能较慢。')
    else
      abort 'usage: clean-generated.sh [--list | --remove RELATIVE_CACHE_PATH ...]'
    end
  rescue StandardError => error
    abort error.message
  end
end
