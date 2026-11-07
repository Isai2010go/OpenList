package blomp

import (
	"context"
	"fmt"
	"io"
	"mime"
	"path"
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
	if strings.TrimSpace(d.AuthURL) == "" {
		return fmt.Errorf("Blomp: Auth URL is required; enter the Keystone v2 URL supplied by Blomp")
	}
	c := &swift.Connection{
		UserName: d.Username,
		ApiKey: d.Password,
		AuthUrl: strings.TrimSpace(d.AuthURL),
		AuthVersion: 2,
		Tenant: "storage",
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
	container, prefix, ok := splitStoragePath(parentDir.GetPath())
	if !ok { return nil, fmt.Errorf("cannot create a container through this operation") }
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
	sc, so, ok := splitStoragePath(srcObj.GetPath()); if !ok { return nil, fmt.Errorf("invalid source path") }
	dc, dp, ok := splitStoragePath(dstDir.GetPath()); if !ok { return nil, fmt.Errorf("invalid destination path") }
	target := path.Join(dp, srcObj.GetName())
	if srcObj.IsDir() { target += "/" }
	var err error
	if move { err = d.conn.ObjectMove(ctx, sc, so, dc, target) } else { _, err = d.conn.ObjectCopy(ctx, sc, so, dc, target, nil) }
	if err != nil { return nil, err }
	return &model.Object{ID:dc+"/"+target, Path:path.Join(dstDir.GetPath(), srcObj.GetName()), Name:srcObj.GetName(), Size:srcObj.GetSize(), IsFolder:srcObj.IsDir(), Modified:time.Now()}, nil
}

func (d *Blomp) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	container, object, ok := splitStoragePath(srcObj.GetPath()); if !ok { return nil, fmt.Errorf("invalid source path") }
	parent := path.Dir(object)
	target := path.Join(parent, newName)
	if srcObj.IsDir() { target += "/" }
	if err := d.conn.ObjectMove(ctx, container, object, container, target); err != nil { return nil, err }
	return &model.Object{ID:container+"/"+target, Path:path.Join(path.Dir(srcObj.GetPath()), newName), Name:newName, Size:srcObj.GetSize(), IsFolder:srcObj.IsDir(), Modified:time.Now()}, nil
}

func (d *Blomp) Remove(ctx context.Context, obj model.Obj) error {
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
	container, prefix, ok := splitStoragePath(dstDir.GetPath()); if !ok { return nil, fmt.Errorf("invalid destination path") }
	object := path.Join(prefix, file.GetName())
	var reader io.Reader = driver.NewLimitedUploadStream(ctx, file)
	if up != nil && file.GetSize() > 0 {
		reader = io.TeeReader(reader, driver.NewProgress(file.GetSize(), up))
	}
	contentType := file.GetMimetype()
	if contentType == "" { contentType = mime.TypeByExtension(path.Ext(file.GetName())) }
	if contentType == "" { contentType = "application/octet-stream" }
	if _, err := d.conn.ObjectPut(ctx, container, object, reader, false, "", contentType, nil); err != nil {
		return nil, fmt.Errorf("upload to Blomp: %w", err)
	}
	return &model.Object{ID:container+"/"+object, Path:path.Join(dstDir.GetPath(), file.GetName()), Name:file.GetName(), Size:file.GetSize(), Modified:time.Now()}, nil
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
