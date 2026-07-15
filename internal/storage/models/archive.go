package models

import (
	"encoding/json"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/ddvk/rmfakecloud/internal/storage"
	"github.com/ddvk/rmfakecloud/internal/storage/exporter"
	"github.com/juruen/rmapi/archive"
	"github.com/juruen/rmapi/encoding/rm"
	log "github.com/sirupsen/logrus"
)

// ArchiveFromHashDoc reads an archive
func ArchiveFromHashDoc(doc *HashDoc, rs RemoteStorage) (*exporter.MyArchive, error) {
	uuid := doc.EntryName
	a := exporter.MyArchive{
		Zip: archive.Zip{
			UUID: uuid,
		},
	}

	pageMap := make(map[string]string)
	for _, f := range doc.Files {
		filext := path.Ext(f.EntryName)
		name := strings.TrimSuffix(path.Base(f.EntryName), filext)
		switch filext {
		case storage.ContentFileExt:
			blob, err := rs.GetReader(f.Hash)
			if err != nil {
				return nil, err
			}
			defer blob.Close()
			contentBytes, err := io.ReadAll(blob)
			if err != nil {
				return nil, err
			}
			err = json.Unmarshal(contentBytes, &a.Content)
			if err != nil {
				return nil, err
			}
		case storage.EpubFileExt:
			fallthrough
		case storage.PdfFileExt:
			blob, err := rs.GetReader(f.Hash)
			if err != nil {
				return nil, err
			}
			// defer blob.Close()
			// contentBytes, err := ioutil.ReadAll(blob)
			// if err != nil {
			// 	return nil, err
			// }
			// a.Payload = contentBytes
			//HACK:
			a.PayloadReader = blob.(io.ReadSeekCloser)

		case ".json":
			//metadata
		case storage.RmFileExt:
			log.Debug("adding page ", name)
			pageMap[name] = f.Hash
		}
	}

	// Build ordered page list:
	// 1. If cPages is populated (firmware 3.0+, Quick Sheets), sort by idx and skip deleted
	// 2. If the old pages list is populated, use it
	// 3. Otherwise, fall back to all .rm files sorted by name
	var orderedPageIDs []string
	if a.Content.CPagesData != nil && len(a.Content.CPagesData.Pages) > 0 {
		// Copy and sort by idx.Value (CRDT order) to ensure correct page sequence
		cpages := make([]archive.CPageEntry, len(a.Content.CPagesData.Pages))
		copy(cpages, a.Content.CPagesData.Pages)
		sort.Slice(cpages, func(i, j int) bool {
			return cpages[i].Idx.Value < cpages[j].Idx.Value
		})
		for _, p := range cpages {
			// Skip deleted pages
			if p.Deleted != nil && p.Deleted.Value != 0 {
				continue
			}
			orderedPageIDs = append(orderedPageIDs, p.ID)
		}
	} else if len(a.Content.Pages) > 0 {
		orderedPageIDs = append(orderedPageIDs, a.Content.Pages...)
	}

	if len(orderedPageIDs) > 0 {
		for _, p := range orderedPageIDs {
			if hash, ok := pageMap[p]; ok {
				log.Debug("page ", hash)
				reader, err := rs.GetReader(hash)
				if err != nil {
					return nil, err
				}
				pageBin, err := io.ReadAll(reader)
				if err != nil {
					return nil, err
				}
				rmpage := rm.New()
				err = rmpage.UnmarshalBinary(pageBin)
				if err != nil {
					return nil, err
				}

				page := archive.Page{
					Data:     rmpage,
					Pagedata: "Blank",
				}
				a.Pages = append(a.Pages, page)
			}
		}
	} else {
		// Fallback: no pages list in content file, use all .rm files
		pageNames := make([]string, 0, len(pageMap))
		for name := range pageMap {
			pageNames = append(pageNames, name)
		}
		sort.Strings(pageNames)
		for _, name := range pageNames {
			hash := pageMap[name]
			log.Debug("page (fallback) ", hash)
			reader, err := rs.GetReader(hash)
			if err != nil {
				return nil, err
			}
			pageBin, err := io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
			rmpage := rm.New()
			err = rmpage.UnmarshalBinary(pageBin)
			if err != nil {
				return nil, err
			}

			page := archive.Page{
				Data:     rmpage,
				Pagedata: "Blank",
			}
			a.Pages = append(a.Pages, page)
		}
	}

	return &a, nil
}
