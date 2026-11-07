package blomp

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/ncw/swift/v2"
)

type Blomp struct {
	model.Storage
	Addition
	conn *swift.Connection
}

func (d *Blomp) Config() driver.Config { return config }
func (d *Blomp) GetAddition() driver.Additional { return &d.Addition }

func (d *Blomp) Init(ctx context.Context) error {
	if strings.TrimSpace(d.Username) == "" || d.Password == "" {
		return fmt.Errorf("Blomp: username and password are required")
	}
	authURL := strings.TrimSpace(d.AuthURL)
	if authURL == "" {
		return fmt.Errorf("Blomp: Auth URL is required; enter the Keystone v2 URL supplied by Blomp")
	}
	parsedAuthURL, err := url.ParseRequestURI(authURL)
	if err != nil || parsedAuthURL.Scheme != "https" || parsedAuthURL.Host == "" {
		return fmt.Errorf("Blomp: Auth URL must be a valid HTTPS Keystone v2 endpoint")
	}
	c := &swift.Connection{
		UserName: d.Username,
		ApiKey: d.Password,
		AuthUrl: authURL,
		AuthVersion: 2,
		Tenant: d.Tenant,
		Region: d.Region,
		ConnectTimeout: 15 * time.Second,
		Timeout: 2 * time.Minute,
		UserAgent: "OpenList-Blomp/1.0",
	}
	if err := c.Authenticate(ctx); err != nil {
		return fmt.Errorf("Blomp authentication failed: %w", err)
	}
	if c.StorageUrl == "" {
		return fmt.Errorf("Blomp authentication succeeded but no object-store endpoint was returned")
	}
	if strings.HasPrefix(strings.ToLower(c.StorageUrl), "http://") && !d.AllowHTTP {
		return fmt.Errorf("Blomp returned an HTTP storage endpoint; enable Allow HTTP only if you accept unencrypted token and file transfer")
	}
	if !strings.HasPrefix(strings.ToLower(c.StorageUrl), "https://") && !strings.HasPrefix(strings.ToLower(c.StorageUrl), "http://") {
		return fmt.Errorf("Blomp returned an unsupported storage endpoint scheme")
	}
	d.conn = c
	// Authentication alone is not enough: verify that the storage proxy is reachable.
	if _, err := c.ContainerNames(ctx, &swift.ContainersOpts{Limit: 1}); err != nil {
		return fmt.Errorf("Blomp storage endpoint check failed (authentication may have succeeded): %w", err)
	}
	return nil
}

func (d *Blomp) Drop(ctx context.Context) error {
	d.conn = nil
	return nil
}

func (d *Blomp) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if d.conn == nil { return nil, fmt.Errorf("Blomp is not initialized") }
	if dir.GetPath() == "" || dir.GetPath() == "/" {
		names, err := d.conn.ContainerNames(ctx, &swift.ContainersOpts{})
		if err != nil { return nil, fmt.Errorf("list Blomp containers: %w", err) }
		out := make([]model.Obj, 0, len(names))
		for _, name := range names {
			out = append(out, &model.Object{ID:name, Path:name, Name:name, IsFolder:true})
		}
		return out, nil
	}
	container, prefix, ok := splitStoragePath(dir.GetPath())
	if !ok { return nil, fmt.Errorf("invalid Blomp path: %q", dir.GetPath()) }
	if prefix != "" && !strings.HasSuffix(prefix, "/") { prefix += "/" }
	opts := &swift.ObjectsOpts{Prefix:prefix, Delimiter:"/", Limit:10000}
	objects, err := d.conn.ObjectsAll(ctx, container, opts)
	if err != nil { return nil, fmt.Errorf("list Blomp container %q: %w", container, err) }
	out := make([]model.Obj, 0, len(objects))
	seen := make(map[string]struct{})
	for _, item := range objects {
		name := strings.TrimPrefix(item.Name, prefix)
		if name == "" { continue }
		isDir := item.PseudoDirectory || strings.HasSuffix(name, "/")
		if isDir {
			name = strings.TrimSuffix(name, "/")
		} else if strings.Contains(name, "/") {
			// A server may ignore Delimiter; expose only the next path segment.
			name = strings.SplitN(name, "/", 2)[0]
			isDir = true
		}
		if name == "" { continue }
		if _, exists := seen[name]; exists { continue }
		seen[name] = struct{}{}
		full := path.Join(dir.GetPath(), name)
		obj := &model.Object{ID:container+"/"+strings.TrimSuffix(item.Name, "/"), Path:full, Name:name, Size:item.Bytes, Modified:item.LastModified, IsFolder:isDir}
		out = append(out, obj)
	}
	model.SortFiles(out, "", "")
	return out, nil
}

func (d *Blomp) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if d.conn == nil { return nil, fmt.Errorf("Blomp is not initialized") }
	container, object, ok := splitStoragePath(file.GetPath())
	if !ok || file.IsDir() { return nil, fmt.Errorf("invalid Blomp file path") }
	return &model.Link{
		RangeReader: &blompRangeReader{conn:d.conn, container:container, object:object},
		ContentLength:file.GetSize(),
	}, nil
}

type blompRangeReader struct {
	conn *swift.Connection
	container, object string
}

func (r *blompRangeReader) RangeRead(ctx context.Context, rg http_range.Range) (io.ReadCloser, error) {
	if rg.Length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	headers := swift.Headers{}
	if rg.Start > 0 || rg.Length >= 0 {
		end := ""
		if rg.Length >= 0 { end = fmt.Sprintf("%d", rg.Start+rg.Length-1) }
		headers["Range"] = fmt.Sprintf("bytes=%d-%s", rg.Start, end)
	}
	file, _, err := r.conn.ObjectOpen(ctx, r.container, r.object, false, headers)
	if err != nil { return nil, fmt.Errorf("read Blomp object: %w", err) }
	return file, nil
}

func (d *Blomp) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	if d.conn == nil { return nil, fmt.Errorf("Blomp is not initialized") }
	if !validName(dirName) { return nil, fmt.Errorf("invalid Blomp directory name %q", dirName) }
	container, prefix, ok := splitStoragePath(parentDir.GetPath())
	if !ok {
		if err := d.conn.ContainerCreate(ctx, dirName, nil); err != nil { return nil, err }
		return &model.Object{ID:dirName, Path:dirName, Name:dirName, IsFolder:true}, nil
	}
	name := strings.TrimSuffix(path.Join(prefix, dirName), "/") + "/"
	if err := d.conn.ObjectPutString(ctx, container, name, "", "application/directory"); err != nil { return nil, err }
	return &model.Object{ID:container+"/"+name, Path:path.Join(parentDir.GetPath(), dirName), Name:dirName, IsFolder:true}, nil
}

func (d *Blomp) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	return d.moveOrCopy(ctx, srcObj, dstDir, true)
}

func (d *Blomp) Copy(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	return d.moveOrCopy(ctx, srcObj, dstDir, false)
}

func (d *Blomp) moveOrCopy(ctx context.Context, srcObj, dstDir model.Obj, move bool) (model.Obj, error) {
	if d.conn == nil {
		return nil, fmt.Errorf("Blomp is not initialized")
	}
	srcContainer, srcObject, ok := splitStoragePath(srcObj.GetPath())
	if !ok {
		return nil, fmt.Errorf("invalid Blomp source path %q", srcObj.GetPath())
	}
	dstContainer, dstPrefix, ok := splitStoragePath(dstDir.GetPath())
	if !ok {
		return nil, fmt.Errorf("invalid Blomp destination path %q", dstDir.GetPath())
	}
	targetName := path.Join(dstPrefix, srcObj.GetName())
	if srcObj.IsDir() {
		targetName = strings.TrimSuffix(targetName, "/") + "/"
	}
	if err := d.transferObject(ctx, srcContainer, srcObject, dstContainer, targetName, srcObj.IsDir(), move); err != nil {
		return nil, err
	}
	return &model.Object{
		ID: dstContainer + "/" + strings.TrimSuffix(targetName, "/"),
		Path: path.Join(dstDir.GetPath(), srcObj.GetName()),
		Name: srcObj.GetName(),
		Size: srcObj.GetSize(),
		IsFolder: srcObj.IsDir(),
		Modified: time.Now(),
	}, nil
}

// transferObject copies a file or an entire Swift pseudo-directory. For a
// directory, all descendants are copied before any source object is removed.
func (d *Blomp) transferObject(ctx context.Context, srcContainer, srcObject, dstContainer, dstObject string, isDir, move bool) error {
	if !isDir && srcContainer == dstContainer && srcObject == dstObject {
		return nil
	}
	if isDir && srcContainer == dstContainer {
		srcPrefix := strings.TrimSuffix(srcObject, "/") + "/"
		dstPrefix := strings.TrimSuffix(dstObject, "/") + "/"
		if srcPrefix == dstPrefix {
			return nil
		}
		if strings.HasPrefix(dstPrefix, srcPrefix) {
			return fmt.Errorf("cannot copy or move a directory into itself or one of its descendants")
		}
	}
	if !isDir {
		if move {
			return d.conn.ObjectMove(ctx, srcContainer, srcObject, dstContainer, dstObject)
		}
		_, err := d.conn.ObjectCopy(ctx, srcContainer, srcObject, dstContainer, dstObject, nil)
		return err
	}

	srcPrefix := strings.TrimSuffix(srcObject, "/") + "/"
	dstPrefix := strings.TrimSuffix(dstObject, "/") + "/"
	items, err := d.conn.ObjectsAll(ctx, srcContainer, &swift.ObjectsOpts{
		Prefix: srcPrefix,
		Limit:  1000,
	})
	if err != nil {
		return fmt.Errorf("list Blomp directory before transfer: %w", err)
	}
	if len(items) == 0 {
		// Preserve an empty directory even if the backend does not materialize
		// pseudo-directories as objects.
		return d.conn.ObjectPutString(ctx, dstContainer, dstPrefix, "", "application/directory")
	}

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative := strings.TrimPrefix(item.Name, srcPrefix)
		if relative == "" {
			continue
		}
		if _, err := d.conn.ObjectCopy(ctx, srcContainer, item.Name, dstContainer, dstPrefix+relative, nil); err != nil {
			return fmt.Errorf("copy Blomp directory object %q: %w", item.Name, err)
		}
	}
	if !move {
		return nil
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.conn.ObjectDelete(ctx, srcContainer, item.Name); err != nil {
			return fmt.Errorf("remove source object %q after directory copy: %w", item.Name, err)
		}
	}
	return nil
}

func (d *Blomp) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	if d.conn == nil {
		return nil, fmt.Errorf("Blomp is not initialized")
	}
	if newName == "" || newName == "." || newName == ".." || strings.ContainsAny(newName, "/\\\\") {
		return nil, fmt.Errorf("invalid Blomp object name %q", newName)
	}
	container, object, ok := splitStoragePath(srcObj.GetPath())
	if !ok {
		return nil, fmt.Errorf("invalid Blomp source path %q", srcObj.GetPath())
	}
	target := path.Join(path.Dir(object), newName)
	if srcObj.IsDir() {
		target = strings.TrimSuffix(target, "/") + "/"
	}
	if err := d.transferObject(ctx, container, object, container, target, srcObj.IsDir(), true); err != nil {
		return nil, err
	}
	return &model.Object{
		ID: container + "/" + strings.TrimSuffix(target, "/"),
		Path: path.Join(path.Dir(srcObj.GetPath()), newName),
		Name: newName,
		Size: srcObj.GetSize(),
		IsFolder: srcObj.IsDir(),
		Modified: time.Now(),
	}, nil
}

func (d *Blomp) Remove(ctx context.Context, obj model.Obj) error {
	if d.conn == nil { return fmt.Errorf("Blomp is not initialized") }
	container, object, ok := splitStoragePath(obj.GetPath()); if !ok { return fmt.Errorf("cannot remove container root") }
	if obj.IsDir() {
		items, err := d.conn.ObjectsAll(ctx, container, &swift.ObjectsOpts{Prefix:strings.TrimSuffix(object, "/")+"/", Limit:10000})
		if err != nil { return err }
		for _, item := range items { if err := d.conn.ObjectDelete(ctx, container, item.Name); err != nil { return err } }
		return nil
	}
	return d.conn.ObjectDelete(ctx, container, object)
}

func (d *Blomp) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	if d.conn == nil { return nil, fmt.Errorf("Blomp is not initialized") }
	if !validName(file.GetName()) { return nil, fmt.Errorf("invalid Blomp file name %q", file.GetName()) }
	container, prefix, ok := splitStoragePath(dstDir.GetPath()); if !ok { return nil, fmt.Errorf("invalid destination path") }
	object := path.Join(prefix, file.GetName())
	var reader io.Reader = driver.NewLimitedUploadStream(ctx, file)
	if up != nil && file.GetSize() > 0 {
		reader = io.TeeReader(reader, driver.NewProgress(file.GetSize(), up))
	}
	contentType := file.GetMimetype()
	if contentType == "" { contentType = mime.TypeByExtension(path.Ext(file.GetName())) }
	if contentType == "" { contentType = "application/octet-stream" }
	headers := swift.Headers{}
	if size := file.GetSize(); size >= 0 {
		headers["Content-Length"] = strconv.FormatInt(size, 10)
	}
	if _, err := d.conn.ObjectPut(ctx, container, object, reader, true, "", contentType, headers); err != nil {
		return nil, fmt.Errorf("upload to Blomp: %w", err)
	}
	return &model.Object{ID:container+"/"+object, Path:path.Join(dstDir.GetPath(), file.GetName()), Name:file.GetName(), Size:file.GetSize(), Modified:time.Now()}, nil
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\\")
}

func splitStoragePath(p string) (container, object string, ok bool) {
	p = strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
	if p == "" { return "", "", false }
	parts := strings.SplitN(p, "/", 2)
	container = parts[0]
	if len(parts) == 2 { object = parts[1] }
	return container, object, true
}

var _ driver.Driver = (*Blomp)(nil)
