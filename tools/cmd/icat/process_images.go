// License: GPLv3 Copyright: 2022, Kovid Goyal, <kovid at kovidgoyal.net>

package icat

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"kitty/tools/tty"
	"kitty/tools/utils"
	"kitty/tools/utils/shm"
)

var _ = fmt.Print

type BytesBuf struct {
	data []byte
	pos  int64
}

func (self *BytesBuf) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		self.pos = offset
	case io.SeekCurrent:
		self.pos += offset
	case io.SeekEnd:
		self.pos = int64(len(self.data)) + offset
	default:
		return self.pos, fmt.Errorf("Unknown value for whence: %#v", whence)
	}
	self.pos = utils.Max(0, utils.Min(self.pos, int64(len(self.data))))
	return self.pos, nil
}

func (self *BytesBuf) Read(p []byte) (n int, err error) {
	nb := utils.Min(int64(len(p)), int64(len(self.data))-self.pos)
	if nb == 0 {
		err = io.EOF
	} else {
		n = copy(p, self.data[self.pos:self.pos+nb])
		self.pos += nb
	}
	return
}

func (self *BytesBuf) Close() error {
	self.data = nil
	self.pos = 0
	return nil
}

type input_arg struct {
	arg         string
	value       string
	is_http_url bool
}

func is_http_url(arg string) bool {
	return strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://")
}

func process_dirs(args ...string) (results []input_arg, err error) {
	results = make([]input_arg, 0, 64)
	if opts.Stdin != "no" && (opts.Stdin == "yes" || !tty.IsTerminal(os.Stdin.Fd())) {
		results = append(results, input_arg{arg: "/dev/stdin"})
	}
	for _, arg := range args {
		if arg != "" {
			if is_http_url(arg) {
				results = append(results, input_arg{arg: arg, value: arg, is_http_url: true})
			} else {
				if strings.HasPrefix(arg, "file://") {
					u, err := url.Parse(arg)
					if err != nil {
						return nil, &fs.PathError{Op: "Parse", Path: arg, Err: err}
					}
					arg = u.Path
				}
				s, err := os.Stat(arg)
				if err != nil {
					return nil, &fs.PathError{Op: "Stat", Path: arg, Err: err}
				}
				if s.IsDir() {
					filepath.WalkDir(arg, func(path string, d fs.DirEntry, walk_err error) error {
						if walk_err != nil {
							if d == nil {
								err = &fs.PathError{Op: "Stat", Path: arg, Err: walk_err}
							}
							return walk_err
						}
						if !d.IsDir() {
							mt := utils.GuessMimeType(path)
							if strings.HasPrefix(mt, "image/") {
								results = append(results, input_arg{arg: arg, value: path})
							}
						}
						return nil
					})
				} else {
					results = append(results, input_arg{arg: arg, value: arg})
				}
			}
		}
	}
	return results, nil
}

type opened_input struct {
	file           io.ReadSeekCloser
	name_to_unlink string
}

func (self *opened_input) Rewind() {
	if self.file != nil {
		self.file.Seek(0, io.SeekStart)
	}
}

func (self *opened_input) Release() {
	if self.file != nil {
		self.file.Close()
		self.file = nil
	}
	if self.name_to_unlink != "" {
		os.Remove(self.name_to_unlink)
		self.name_to_unlink = ""
	}
}

type image_frame struct {
	filename              string
	shm                   shm.MMap
	in_memory_bytes       []byte
	filename_is_temporary bool
	width, height         int
}

type image_data struct {
	canvas_width, canvas_height       int
	format_uppercase                  string
	available_width, available_height int
	needs_scaling, needs_conversion   bool
	frames                            []*image_frame
	image_number                      uint32

	// for error reporting
	err         error
	source_name string
}

func set_basic_metadata(imgd *image_data) {
	if imgd.frames == nil {
		imgd.frames = make([]*image_frame, 0, 32)
	}
	imgd.available_width = int(screen_size.WidthPx)
	imgd.available_height = 10 * imgd.canvas_height
	if place != nil {
		imgd.available_width = place.width * int(screen_size.CellWidth)
		imgd.available_height = place.height * int(screen_size.CellHeight)
	}
	imgd.needs_scaling = imgd.canvas_width > imgd.available_width || imgd.canvas_height > imgd.available_height || opts.ScaleUp
	imgd.needs_conversion = imgd.needs_scaling || remove_alpha != nil || flip || flop || imgd.format_uppercase != "PNG"
}

func send_output(imgd *image_data) {
	output_channel <- imgd
	lp.WakeupMainThread()
}

func report_error(source_name, msg string, err error) {
	imgd := image_data{source_name: source_name, err: fmt.Errorf("%s: %w", msg, err)}
	send_output(&imgd)
}

func make_output_from_input(imgd *image_data, f *opened_input) {
	bb, ok := f.file.(*BytesBuf)
	frame := image_frame{}
	imgd.frames = append(imgd.frames, &frame)
	frame.width = imgd.canvas_width
	frame.height = imgd.canvas_height
	if ok {
		frame.in_memory_bytes = bb.data
	} else {
		frame.filename = f.file.(*os.File).Name()
		if f.name_to_unlink != "" {
			frame.filename_is_temporary = true
			f.name_to_unlink = ""
		}
	}
}

func process_arg(arg input_arg) {
	var f opened_input
	if arg.is_http_url {
		resp, err := http.Get(arg.value)
		if err != nil {
			report_error(arg.value, "Could not get", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			report_error(arg.value, "Could not get", fmt.Errorf("bad status: %v", resp.Status))
			return
		}
		dest := bytes.Buffer{}
		dest.Grow(64 * 1024)
		_, err = io.Copy(&dest, resp.Body)
		if err != nil {
			report_error(arg.value, "Could not download", err)
			return
		}
		f.file = &BytesBuf{data: dest.Bytes()}
	} else if arg.value == "" {
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil {
			report_error("<stdin>", "Could not read from", err)
			return
		}
		f.file = &BytesBuf{data: stdin}
	} else {
		q, err := os.Open(arg.value)
		if err != nil {
			report_error(arg.value, "Could not open", err)
			return
		}
		f.file = q
	}
	defer f.Release()
	c, format, err := image.DecodeConfig(f.file)
	f.Rewind()
	imgd := image_data{source_name: arg.value}
	if err != nil {
		report_error(arg.value, "Unknown image format", err)
		return
	}
	if !keep_going.Load() {
		return
	}
	imgd.canvas_width = c.Width
	imgd.canvas_height = c.Height
	imgd.format_uppercase = strings.ToUpper(format)
	set_basic_metadata(&imgd)
	if !imgd.needs_conversion {
		make_output_from_input(&imgd, &f)
		send_output(&imgd)
		return
	}
	err = render_image_with_go(&imgd, &f)
	if err != nil {
		report_error(arg.value, "Could not render image to RGB", err)
		return
	}
	send_output(&imgd)

}

func run_worker() {
	for {
		select {
		case arg := <-files_channel:
			if !keep_going.Load() {
				return
			}
			process_arg(arg)
		default:
			return
		}
	}
}
