package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goagent/pkg/agent"
	"goagent/pkg/permission"
)

// Workspace 固定文件工具的根目录。Close 应在所有工具调用结束后执行。
// 文件访问使用目录句柄；这不限制 bash 的宿主文件系统或网络能力。
type Workspace struct {
	root string
	dir  *os.File
}

func NewWorkspace(root string) (*Workspace, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	dir, err := openWorkspaceRoot(canonical)
	if err != nil {
		return nil, err
	}
	return &Workspace{root: canonical, dir: dir}, nil
}

func (w *Workspace) Root() string { return w.root }
func (w *Workspace) Close() error { return w.dir.Close() }

// CodingTools 返回受工作区约束的文件工具；bash 只固定工作目录，仍需宿主隔离。
// web_search/web_fetch 不操作文件路径，不经工作区包装，但保留权限审批。
func (w *Workspace) CodingTools() []agent.AgentTool {
	bash := Bash().(*bashTool)
	bash.dir = w.root
	return append([]agent.AgentTool{bash, WebSearch(), WebFetch()}, w.fileTools(Read(), Write(), Edit(), MultiEdit(), LS(), Grep(), Glob())...)
}

func (w *Workspace) ReadOnlyTools() []agent.AgentTool {
	return w.fileTools(Read(), LS(), Grep(), Glob())
}

func (w *Workspace) fileTools(list ...agent.AgentTool) []agent.AgentTool {
	for i, tool := range list {
		list[i] = &workspaceTool{AgentTool: tool, workspace: w}
	}
	return list
}

type workspaceTool struct {
	agent.AgentTool
	workspace *Workspace
}

func (t *workspaceTool) IsReadOnly(args json.RawMessage) bool {
	return t.AgentTool.(agent.ReadOnlyTool).IsReadOnly(args)
}

func (t *workspaceTool) CheckPermissions(ctx context.Context, args json.RawMessage) permission.Decision {
	prepared, err := t.PrepareCall(ctx, args)
	if err != nil {
		return permission.Denied(err.Error())
	}
	return prepared.(agent.PermissionCheckedTool).CheckPermissions(ctx, args)
}

// Execute 的直接调用没有审批宿主，仅允许工作区内的只读操作。
func (t *workspaceTool) Execute(ctx context.Context, id string, args json.RawMessage, update func(agent.ToolResult)) (agent.ToolResult, error) {
	prepared, err := t.PrepareCall(ctx, args)
	if err != nil {
		return agent.ToolResult{}, err
	}
	decision := prepared.(agent.PermissionCheckedTool).CheckPermissions(ctx, args)
	if decision.Behavior != permission.Allow {
		return agent.ToolResult{}, fmt.Errorf("file access requires permission: %s", decision.Reason)
	}
	return prepared.Execute(ctx, id, args, update)
}

func (t *workspaceTool) PrepareCall(ctx context.Context, args json.RawMessage) (agent.AgentTool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var values map[string]json.RawMessage
	if err := decodeArgs(args, &values); err != nil {
		return nil, err
	}
	var path string
	if value, ok := values["path"]; ok {
		if err := json.Unmarshal(value, &path); err != nil {
			return nil, err
		}
	}
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.workspace.root, path)
	}
	target, err := resolveWorkspacePath(path)
	if err != nil {
		return nil, fmt.Errorf("resolve file access: %w", err)
	}
	inside := withinDirectory(t.workspace.root, target)
	readOnly := t.IsReadOnly(args)
	decision := t.AgentTool.(agent.PermissionCheckedTool).CheckPermissions(ctx, args)
	if !inside {
		if !readOnly {
			return nil, fmt.Errorf("write outside workspace is denied: %q", target)
		}
		decision = permission.RequiredApproval(fmt.Sprintf("read outside workspace: %q (workspace: %q)", target, t.workspace.root))
	}
	values["path"], _ = json.Marshal(target)
	boundArgs, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	info, _ := os.Lstat(target)
	targetDir := info != nil && info.IsDir()
	return &workspaceCall{workspaceTool: t, target: target, inside: inside, targetDir: targetDir, args: boundArgs, decision: decision}, nil
}

type workspaceCall struct {
	*workspaceTool
	target    string
	inside    bool
	targetDir bool
	args      json.RawMessage
	decision  permission.Decision
}

func (t *workspaceCall) CheckPermissions(context.Context, json.RawMessage) permission.Decision {
	return t.decision
}

func (t *workspaceCall) Execute(ctx context.Context, id string, _ json.RawMessage, update func(agent.ToolResult)) (agent.ToolResult, error) {
	if t.decision.Behavior == permission.Deny {
		return agent.ToolResult{}, fmt.Errorf("%s", t.decision.Reason)
	}
	access := &fileAccess{root: t.workspace.root, dir: t.workspace.dir}
	if !t.targetDir {
		access.only = t.target
	}
	if !t.inside {
		// 单次文件审批只授予该文件；目录审批授予其真实子树，不跟随逃逸链接。
		root := t.target
		if !t.targetDir {
			root = filepath.Dir(t.target)
		}
		dir, err := openWorkspaceRoot(root)
		if err != nil {
			return agent.ToolResult{}, err
		}
		defer dir.Close()
		access.root, access.dir = root, dir
	}
	ctx = context.WithValue(ctx, fileAccessKey{}, access)
	return t.AgentTool.Execute(ctx, id, t.args, update)
}

// resolveWorkspacePath 同时支持尚未创建的文件/父目录；悬空链接和链接环失败关闭。
func resolveWorkspacePath(path string) (string, error) {
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolveWorkspacePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func withinDirectory(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && filepath.IsLocal(rel)
}

type fileAccessKey struct{}
type fileAccess struct {
	root string
	dir  *os.File
	only string
}

func fileAccessError(ctx context.Context, err error) (agent.ToolResult, error) {
	if _, scoped := ctx.Value(fileAccessKey{}).(*fileAccess); scoped {
		return agent.ToolResult{}, err
	}
	return textResult("error: " + err.Error()), nil
}

func (a *fileAccess) relative(path string) (string, error) {
	resolved, err := resolveWorkspacePath(path)
	if err != nil {
		return "", err
	}
	if !withinDirectory(a.root, resolved) || (a.only != "" && resolved != a.only) {
		return "", fmt.Errorf("path is outside the approved file scope: %q", path)
	}
	return filepath.Rel(a.root, resolved)
}

func openFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a, ok := ctx.Value(fileAccessKey{}).(*fileAccess); ok {
		rel, err := a.relative(path)
		if err != nil {
			return nil, err
		}
		f, err := openWorkspaceFile(a.dir, rel, false, false)
		if err != nil {
			return nil, err
		}
		if a.only != "" {
			info, err := f.Stat()
			if err != nil || info.IsDir() {
				f.Close()
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("approved file became a directory: %q", path)
			}
		}
		return f, nil
	}
	return os.Open(path)
}

func readFile(ctx context.Context, path string) ([]byte, error) {
	f, err := openFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func writeFile(ctx context.Context, path string, content []byte, createParents bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a, ok := ctx.Value(fileAccessKey{}).(*fileAccess); ok {
		rel, err := a.relative(path)
		if err != nil {
			return err
		}
		f, err := openWorkspaceFile(a.dir, rel, true, createParents)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := f.Truncate(0); err != nil {
			return err
		}
		_, err = f.Write(content)
		return err
	}
	if createParents {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, content, 0o644)
}

func statFile(ctx context.Context, path string) (os.FileInfo, error) {
	if _, ok := ctx.Value(fileAccessKey{}).(*fileAccess); !ok {
		return os.Stat(path)
	}
	f, err := openFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

func readDirectory(ctx context.Context, path string) ([]os.DirEntry, error) {
	if _, ok := ctx.Value(fileAccessKey{}).(*fileAccess); !ok {
		return os.ReadDir(path)
	}
	f, err := openFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func walkDirectory(ctx context.Context, root string, visit fs.WalkDirFunc) error {
	if _, ok := ctx.Value(fileAccessKey{}).(*fileAccess); !ok {
		return filepath.WalkDir(root, visit)
	}
	info, err := statFile(ctx, root)
	if err != nil {
		return visit(root, nil, err)
	}
	var walk func(string, fs.DirEntry) error
	walk = func(path string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(path, entry, nil); err != nil {
			if err == filepath.SkipDir && entry.IsDir() {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		entries, err := readDirectory(ctx, path)
		if err != nil {
			return visit(path, entry, err)
		}
		for _, child := range entries {
			if err := walk(filepath.Join(path, child.Name()), child); err != nil {
				return err
			}
		}
		return nil
	}
	err = walk(root, fs.FileInfoToDirEntry(info))
	if err == filepath.SkipAll || err == filepath.SkipDir {
		return nil
	}
	return err
}

// 绝对路径只能由安全打开的目录句柄开始遍历，不能在最终打开时重新跟随链接。
func absoluteComponents(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("expected absolute path: %q", path)
	}
	return strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), nil
}
