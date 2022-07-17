/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spdx

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	gitignore "github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/uuid"
	"github.com/nozzle/throttler"
	purl "github.com/package-url/packageurl-go"
	"github.com/sirupsen/logrus"

	"sigs.k8s.io/bom/pkg/license"
	"sigs.k8s.io/bom/pkg/osinfo"
	"sigs.k8s.io/release-utils/util"
)

//counterfeiter:generate . spdxImplementation

type spdxImplementation interface {
	ExtractTarballTmp(string) (string, error)
	ReadArchiveManifest(string) (*ArchiveManifest, error)
	PullImagesToArchive(string, string) (*ImageReferenceInfo, error)
	PackageFromImageTarball(*Options, string) (*Package, error)
	PackageFromTarball(*Options, *TarballOptions, string) (*Package, error)
	PackageFromDirectory(*Options, string) (*Package, error)
	GetDirectoryTree(string) ([]string, error)
	IgnorePatterns(string, []string, bool) ([]gitignore.Pattern, error)
	ApplyIgnorePatterns([]string, []gitignore.Pattern) []string
	GetGoDependencies(string, *Options) ([]*Package, error)
	GetDirectoryLicense(*license.Reader, string, *Options) (*license.License, error)
	LicenseReader(*Options) (*license.Reader, error)
	ImageRefToPackage(string, *Options) (*Package, error)
	AnalyzeImageLayer(string, *Package) error
}

type spdxDefaultImplementation struct{}

// ExtractTarballTmp extracts a tarball to a temporary directory
func (di *spdxDefaultImplementation) ExtractTarballTmp(tarPath string) (tmpDir string, err error) {
	tmpDir, err = os.MkdirTemp(os.TempDir(), "spdx-tar-extract-")
	if err != nil {
		return tmpDir, fmt.Errorf("creating temporary directory for tar extraction: %w", err)
	}

	// Open the tar file
	f, err := os.Open(tarPath)
	if err != nil {
		return tmpDir, fmt.Errorf("opening tarball: %w", err)
	}
	defer f.Close()

	// Read the first bytes to determine if the file is compressed
	var sample [3]byte
	var gzipped bool
	if _, err := io.ReadFull(f, sample[:]); err != nil {
		return "", fmt.Errorf("sampling bytes from file header: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return "", fmt.Errorf("rewinding read pointer: %w", err)
	}

	if sample[0] == 0x1f && sample[1] == 0x8b && sample[2] == 0x08 {
		gzipped = true
	}

	var tr *tar.Reader
	if gzipped {
		gzipReader, err := gzip.NewReader(f)
		if err != nil {
			return "", err
		}
		tr = tar.NewReader(gzipReader)
	} else {
		tr = tar.NewReader(f)
	}
	numFiles := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return tmpDir, fmt.Errorf("reading tarfile %s: %w", tarPath, err)
		}

		if hdr.FileInfo().IsDir() {
			continue
		}

		if strings.HasPrefix(filepath.Base(hdr.FileInfo().Name()), ".wh") {
			logrus.Info("Skipping extraction of whithout file")
			continue
		}

		if err := os.MkdirAll(
			filepath.Join(tmpDir, filepath.Dir(hdr.Name)), os.FileMode(0o755),
		); err != nil {
			return tmpDir, fmt.Errorf("creating image directory structure: %w", err)
		}

		targetFile, err := sanitizeExtractPath(tmpDir, hdr.Name)
		if err != nil {
			return tmpDir, err
		}
		f, err := os.Create(targetFile)
		if err != nil {
			return tmpDir, fmt.Errorf("creating image layer file: %w", err)
		}

		if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
			f.Close()
			if err == io.EOF {
				break
			}

			return tmpDir, fmt.Errorf("extracting image data: %w", err)
		}
		f.Close()

		numFiles++
	}

	logrus.Infof("Successfully extracted %d files from image tarball %s", numFiles, tarPath)
	return tmpDir, err
}

// fix gosec G305: File traversal when extracting zip/tar archive
// more context: https://snyk.io/research/zip-slip-vulnerability
func sanitizeExtractPath(tmpDir, filePath string) (string, error) {
	destpath := filepath.Join(tmpDir, filePath)
	if !strings.HasPrefix(destpath, filepath.Clean(tmpDir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s: illegal file path", filePath)
	}

	return destpath, nil
}

// readArchiveManifest extracts the manifest json from an image tar
//    archive and returns the data as a struct
func (di *spdxDefaultImplementation) ReadArchiveManifest(manifestPath string) (manifest *ArchiveManifest, err error) {
	// Check that we have the archive manifest.json file
	if !util.Exists(manifestPath) {
		return manifest, errors.New("unable to find manifest file " + manifestPath)
	}

	// Parse the json file
	manifestData := []ArchiveManifest{}
	manifestJSON, err := os.ReadFile(manifestPath)
	if err != nil {
		return manifest, fmt.Errorf("unable to read from tarfile: %w", err)
	}
	if err := json.Unmarshal(manifestJSON, &manifestData); err != nil {
		fmt.Println(string(manifestJSON))
		return manifest, fmt.Errorf("unmarshalling image manifest: %w", err)
	}
	return &manifestData[0], nil
}

// getImageReferences gets a reference string and returns all image
// references from it
func getImageReferences(referenceString string) (*ImageReferenceInfo, error) {
	ref, err := name.ParseReference(referenceString)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference %s: %w", referenceString, err)
	}

	//	ref, err := name.ParseReference(referenceString)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference %s: %w", referenceString, err)
	}

	images := &ImageReferenceInfo{
		Images: []ImageReferenceInfo{},
	}

	descr, err := remote.Get(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, fmt.Errorf("fetching remote descriptor: %w", err)
	}

	// If the reference is not an image, it has to work as a tag
	tag, tok := descr.Ref.(name.Tag)
	dig, dok := descr.Ref.(name.Digest)
	if !tok && !dok {
		return nil, fmt.Errorf("could not cast tag or digest from reference %s: %w", referenceString, err)
	}

	// Add the main digest
	images.Digest = fmt.Sprintf(
		"%s/%s@%s:%s",
		tag.RegistryStr(), tag.RepositoryStr(),
		descr.Digest.Algorithm, descr.Digest.Hex,
	)

	// If the reference points to an image, return it
	if descr.MediaType.IsImage() {
		logrus.Infof("Reference %s points to a single image", referenceString)
		// Check if we can get an image
		im, err := descr.Image()
		if err != nil {
			return nil, fmt.Errorf("getting image from descriptor: %w", err)
		}

		imageDigest, err := im.Digest()
		if err != nil {
			return nil, fmt.Errorf("while calculating image digest: %w", err)
		}

		// If we could not turn the reference to digest then we synthesize one
		if tok && !dok {
			dig, err = name.NewDigest(
				fmt.Sprintf(
					"%s/%s@%s:%s",
					tag.RegistryStr(), tag.RepositoryStr(),
					imageDigest.Algorithm, imageDigest.Hex,
				),
			)
			if err != nil {
				return nil, fmt.Errorf("building single image digest: %w", err)
			}
		}

		logrus.Infof("Adding image digest %s from reference", dig.String())
		images.Digest = dig.String()
		mt, err := im.MediaType()
		if err == nil {
			images.MediaType = string(mt)
		}
		conf, err := im.ConfigFile()
		if err == nil {
			images.Arch = conf.Architecture
			images.OS = conf.OS
		}
		return images, nil
	}

	// Get the image index
	index, err := descr.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("getting image index for %s: %w", referenceString, err)
	}
	indexManifest, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("getting index manifest from %s: %w", referenceString, err)
	}
	logrus.Infof("Reference image index points to %d manifests", len(indexManifest.Manifests))
	images.MediaType = string(indexManifest.MediaType)

	for _, manifest := range indexManifest.Manifests {
		archImgDigest, err := name.NewDigest(
			fmt.Sprintf(
				"%s/%s@%s:%s",
				tag.RegistryStr(), tag.RepositoryStr(),
				manifest.Digest.Algorithm, manifest.Digest.Hex,
			))
		if err != nil {
			return nil, fmt.Errorf("generating digest for image: %w", err)
		}

		arch, osid := "", ""
		if manifest.Platform != nil {
			arch = manifest.Platform.Architecture
			osid = manifest.Platform.OS
		}

		logrus.Infof("Adding image %s (%s/%s)", archImgDigest, arch, osid)

		images.Images = append(images.Images,
			ImageReferenceInfo{
				Digest:    archImgDigest.String(),
				MediaType: string(manifest.MediaType),
				Arch:      arch,
				OS:        osid,
			})
	}
	return images, nil
}

func PullImageToArchive(referenceString, path string) error {
	ref, err := name.ParseReference(referenceString)
	if err != nil {
		return fmt.Errorf("parsing reference %s: %w", referenceString, err)
	}

	// Get the image from the reference:
	img, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return fmt.Errorf("getting image: %w", err)
	}

	if err := tarball.WriteToFile(path, ref, img); err != nil {
		return fmt.Errorf("writing image to disk: %w", err)
	}
	return nil
}

// PullImagesToArchive takes an image reference (a tag or a digest)
// and writes it into a docker tar archive in path
func (di *spdxDefaultImplementation) PullImagesToArchive(
	referenceString, path string,
) (references *ImageReferenceInfo, err error) {
	// Get the image references from the index
	references, err = getImageReferences(referenceString)
	if err != nil {
		return nil, err
	}

	if !util.Exists(path) {
		if err := os.MkdirAll(path, os.FileMode(0o755)); err != nil {
			return nil, fmt.Errorf("creating image directory: %w", err)
		}
	}

	// Populate a new image reference set with the archive data
	newrefs := *references
	newrefs.Images = []ImageReferenceInfo{}

	// Download 4 arches at once
	t := throttler.New(4, len(references.Images))
	mtx := sync.Mutex{}

	for _, refData := range references.Images {
		go func(r ImageReferenceInfo) {
			ref, err := name.ParseReference(r.Digest)
			if err != nil {
				t.Done(fmt.Errorf("parsing reference %s: %w", r.Digest, err))
				return
			}

			d, ok := ref.(name.Digest)
			if !ok {
				t.Done(errors.New("reference is not a tag or digest"))
				return
			}

			p := strings.Split(d.DigestStr(), ":")
			if len(p) < 2 {
				t.Done(fmt.Errorf("unable to parse digest string %s", d.DigestStr()))
				return
			}
			tarPath := filepath.Join(path, p[1]+".tar")

			logrus.Debugf("Downloading %s from remote registry to %s", r.Digest, tarPath)

			// Download image from remote
			img, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
			if err != nil {
				t.Done(fmt.Errorf("getting image from remote: %w", err))
				return
			}

			// Write image to tar archive
			if err := tarball.MultiWriteToFile(
				tarPath, map[name.Tag]v1.Image{d.Repository.Tag(p[1]): img},
			); err != nil {
				t.Done(fmt.Errorf("writing image to disk: %w", err))
				return
			}

			mtx.Lock()
			r.Archive = tarPath
			newrefs.Images = append(newrefs.Images, r)
			mtx.Unlock()
			t.Done(nil)

		}(refData)
		t.Throttle()
	}
	if err := t.Err(); err != nil {
		return nil, err
	}
	return &newrefs, nil
}

// PackageFromTarball builds a SPDX package from the contents of a tarball
func (di *spdxDefaultImplementation) PackageFromTarball(
	opts *Options, tarOpts *TarballOptions, tarFile string,
) (pkg *Package, err error) {
	logrus.Infof("Generating SPDX package from tarball %s", tarFile)

	if tarOpts.AddFiles {
		// Estract the tarball
		tmp, err := di.ExtractTarballTmp(tarFile)
		if err != nil {
			return nil, fmt.Errorf("extracting tarball to temporary archive: %w", err)
		}
		defer os.RemoveAll(tmp)
		pkg, err = di.PackageFromDirectory(opts, tmp)
		if err != nil {
			return nil, fmt.Errorf("generating package from tar contents: %w", err)
		}
	} else {
		pkg = NewPackage()
	}
	// Set the extract dir option. This makes the package to remove
	// the tempdir prefix from the document paths:
	pkg.Options().WorkDir = tarOpts.ExtractDir
	if err := pkg.ReadSourceFile(tarFile); err != nil {
		return nil, fmt.Errorf("reading source file %s: %w", tarFile, err)
	}
	// Build the ID and the filename from the tarball name
	return pkg, nil
}

// GetDirectoryTree traverses a directory and return a slice of strings with all files
func (di *spdxDefaultImplementation) GetDirectoryTree(dirPath string) ([]string, error) {
	fileList := []string{}

	if err := fs.WalkDir(os.DirFS(dirPath), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		if d.Type() == os.ModeSymlink {
			return nil
		}

		fileList = append(fileList, path)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("buiding directory tree: %w", err)
	}
	return fileList, nil
}

// IgnorePatterns return a list of gitignore patterns
func (di *spdxDefaultImplementation) IgnorePatterns(
	dirPath string, extraPatterns []string, skipGitIgnore bool,
) ([]gitignore.Pattern, error) {
	patterns := []gitignore.Pattern{}
	for _, s := range extraPatterns {
		patterns = append(patterns, gitignore.ParsePattern(s, nil))
	}

	if skipGitIgnore {
		logrus.Debug("Not using patterns in .gitignore")
		return patterns, nil
	}

	if util.Exists(filepath.Join(dirPath, gitIgnoreFile)) {
		f, err := os.Open(filepath.Join(dirPath, gitIgnoreFile))
		if err != nil {
			return nil, fmt.Errorf("opening gitignore file: %w", err)
		}
		defer f.Close()

		// When using .gitignore files, we alwas add the .git directory
		// to match git's behavior
		patterns = append(patterns, gitignore.ParsePattern(".git/", nil))

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			s := scanner.Text()
			if !strings.HasPrefix(s, "#") && len(strings.TrimSpace(s)) > 0 {
				logrus.Debugf("Loaded .gitignore pattern: >>%s<<", s)
				patterns = append(patterns, gitignore.ParsePattern(s, nil))
			}
		}
	}

	logrus.Debugf(
		"Loaded %d patterns from .gitignore (+ %d extra) at root of directory", len(patterns), len(extraPatterns),
	)
	return patterns, nil
}

// ApplyIgnorePatterns applies the gitignore patterns to a list of files, removing matched
func (di *spdxDefaultImplementation) ApplyIgnorePatterns(
	fileList []string, patterns []gitignore.Pattern,
) (filteredList []string) {
	logrus.Infof(
		"Applying %d ignore patterns to list of %d filenames",
		len(patterns), len(fileList),
	)
	// We will return a new file list
	filteredList = []string{}

	// Build the new gitignore matcher
	matcher := gitignore.NewMatcher(patterns)

	// Cycle all files, removing those matched:
	for _, file := range fileList {
		if matcher.Match(strings.Split(file, string(filepath.Separator)), false) {
			logrus.Debugf("File ignored by .gitignore: %s", file)
		} else {
			filteredList = append(filteredList, file)
		}
	}
	return filteredList
}

// GetGoDependencies opens a Go module and directory and returns the
// dependencies as SPDX packages.
func (di *spdxDefaultImplementation) GetGoDependencies(
	path string, opts *Options,
) (spdxPackages []*Package, err error) {
	// Open the directory as a go module:
	mod, err := NewGoModuleFromPath(path)
	if err != nil {
		return nil, fmt.Errorf("creating a mod from the specified path: %w", err)
	}
	mod.Options().OnlyDirectDeps = opts.OnlyDirectDeps
	mod.Options().ScanLicenses = opts.ScanLicenses

	// Open the module
	if err := mod.Open(); err != nil {
		return nil, fmt.Errorf("opening new module path: %w", err)
	}

	defer func() {
		preErr := err
		err = mod.RemoveDownloads()
		if preErr != nil {
			err = preErr
		}
	}()
	if opts.ScanLicenses {
		if errScan := mod.ScanLicenses(); err != nil {
			return nil, errScan
		}
	}

	spdxPackages = []*Package{}
	for _, goPkg := range mod.Packages {
		spdxPkg, err := goPkg.ToSPDXPackage()
		if err != nil {
			// If a dependency cannot be converted, warn but do not die
			logrus.Error(fmt.Errorf("converting go dependency to spdx package: %w", err))
			continue
		}
		spdxPackages = append(spdxPackages, spdxPkg)
	}

	return spdxPackages, err
}

func (di *spdxDefaultImplementation) LicenseReader(spdxOpts *Options) (*license.Reader, error) {
	opts := license.DefaultReaderOptions
	opts.CacheDir = spdxOpts.LicenseCacheDir
	opts.LicenseDir = spdxOpts.LicenseData
	// Create the new reader
	reader, err := license.NewReaderWithOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("creating reusable license reader: %w", err)
	}
	return reader, nil
}

// GetDirectoryLicense takes a path and scans
// the files in it to determine licensins information
func (di *spdxDefaultImplementation) GetDirectoryLicense(
	reader *license.Reader, path string, spdxOpts *Options,
) (*license.License, error) {
	licenseResult, err := reader.ReadTopLicense(path)
	if err != nil {
		return nil, fmt.Errorf("getting directory license: %w", err)
	}
	if licenseResult == nil {
		logrus.Warnf("License classifier could not find a license for directory: %v", err)
		return nil, nil
	}
	return licenseResult.License, nil
}

// purlFromImage builds a purl from an image reference
func (*spdxDefaultImplementation) purlFromImage(img *ImageReferenceInfo) string {
	// OCI type urls don't have a namespace ref:
	// https://github.com/package-url/purl-spec/blob/master/PURL-TYPES.rst#oci

	imageReference, err := name.ParseReference(img.Reference)
	if err != nil {
		return ""
	}

	digest := ""
	// If we have the digest, skip checking it from the registry
	if _, ok := imageReference.(name.Digest); ok {
		p := strings.Split(imageReference.(name.Digest).String(), "@")
		if len(p) < 2 {
			return ""
		}
		digest = p[1]
	} else {
		digest, err = crane.Digest(img.Reference)
		if err != nil {
			logrus.Error(err)
			return ""
		}
	}

	url := imageReference.Context().Name()
	parts := strings.Split(url, "/")
	if len(parts) < 2 {
		return ""
	}
	imageName := parts[len(parts)-1]
	url = strings.TrimSuffix(url, "/"+imageName)

	// Add the purl qualifgiers:
	mm := map[string]string{
		"repository_url": url,
	}
	if img.Arch != "" {
		mm["arch"] = img.Arch
	}
	if img.OS != "" {
		mm["os"] = img.OS
	}
	if tag, ok := imageReference.(name.Tag); ok {
		mm["tag"] = tag.String()
	}
	if img.MediaType != "" {
		mm["mediaType"] = img.MediaType
	}

	packageurl := purl.NewPackageURL(
		purl.TypeOCI, "", imageName, digest,
		purl.QualifiersFromMap(mm), "",
	)
	return packageurl.String()
}

// ImageRefToPackage Returns a spdx package from an OCI image reference
func (di *spdxDefaultImplementation) ImageRefToPackage(ref string, opts *Options) (*Package, error) {
	tmpdir, err := os.MkdirTemp("", "doc-build-")
	if err != nil {
		return nil, fmt.Errorf("creating temporary workdir in: %w", err)
	}
	defer os.RemoveAll(tmpdir)

	references, err := di.PullImagesToArchive(ref, tmpdir)
	if err != nil {
		return nil, fmt.Errorf("while downloading images to archive: %w", err)
	}

	logrus.Debugf("Reference %s produced %+v", ref, references)

	// If we just got one image and that image is exactly the same
	// reference, return a single package:
	if len(references.Images) == 0 {
		logrus.Infof("Generating single image package for %s", ref)
		p := NewPackage()
		if references.Archive != "" {
			p, err = di.PackageFromImageTarball(opts, references.Archive)
			if err != nil {
				return nil, fmt.Errorf("building package from single image: %w", err)
			}
		}
		packageurl := di.purlFromImage(references)
		if packageurl != "" {
			p.ExternalRefs = append(p.ExternalRefs, ExternalRef{
				Category: "PACKAGE-MANAGER",
				Type:     "purl",
				Locator:  packageurl,
			})
		}
		return p, nil
	}

	// Create the package representing the image tag:
	logrus.Infof("Generating SBOM for multiarch image %s", references.Digest)
	pkg := &Package{}
	indexDigest, err := name.NewDigest(references.Digest)

	if err != nil {
		return nil, fmt.Errorf("parsing digest %s: %w", references.Digest, err)
	}
	pkg.Name = indexDigest.DigestStr()
	pkg.BuildID(pkg.Name)

	if references.Digest != "" {
		pkg.DownloadLocation = references.Digest
	}

	// Now, cycle each image in the index and generate a package from it
	for _, img := range references.Images {
		subpkg, err := di.PackageFromImageTarball(opts, img.Archive)
		if err != nil {
			return nil, fmt.Errorf("adding image variant package: %w", err)
		}

		if img.Arch != "" || img.OS != "" {
			subpkg.Name = ref + " (" + img.Arch
			if img.Arch != "" {
				subpkg.Name += "/"
			}
			subpkg.Name += img.OS + ")"
		} else {
			subpkg.Name = img.Reference
		}

		packageurl := di.purlFromImage(&img)
		if packageurl != "" {
			subpkg.ExternalRefs = append(subpkg.ExternalRefs, ExternalRef{
				Category: "PACKAGE-MANAGER",
				Type:     "purl",
				Locator:  packageurl,
			})
		}

		// Add the package
		pkg.AddRelationship(&Relationship{
			Peer:       subpkg,
			Type:       CONTAINS,
			FullRender: true,
			Comment:    "Container image lager",
		})
		subpkg.AddRelationship(&Relationship{
			Peer:    pkg,
			Type:    VARIANT_OF,
			Comment: "Image index",
		})
	}

	// Add a the topmost package purl
	packageurl := di.purlFromImage(&ImageReferenceInfo{Reference: ref})
	if packageurl != "" {
		pkg.ExternalRefs = append(pkg.ExternalRefs, ExternalRef{
			Category: "PACKAGE-MANAGER",
			Type:     "purl",
			Locator:  packageurl,
		})
	}
	return pkg, nil
}

// PackageFromImageTarball reads an OCI image archive and produces a SPDX
// packafe describing its layers
func (di *spdxDefaultImplementation) PackageFromImageTarball(
	spdxOpts *Options, tarPath string,
) (imagePackage *Package, err error) {
	logrus.Infof("Generating SPDX package from image tarball %s", tarPath)
	if tarPath == "" {
		return nil, fmt.Errorf("tar path empty")
	}

	// Extract all files from tarfile
	tarOpts := &TarballOptions{}

	// If specified, add individual files from the tarball to the
	// spdx package, unless AnalyzeLayers is set because in that
	// case the individual analyzers decide to do that.
	if spdxOpts.AddTarFiles && !spdxOpts.AnalyzeLayers {
		tarOpts.AddFiles = true
	}
	tarOpts.ExtractDir, err = di.ExtractTarballTmp(tarPath)
	if err != nil {
		return nil, fmt.Errorf("extracting tarball to temp dir: %w", err)
	}
	defer os.RemoveAll(tarOpts.ExtractDir)

	// Read the archive manifest json:
	manifest, err := di.ReadArchiveManifest(
		filepath.Join(tarOpts.ExtractDir, archiveManifestFilename),
	)
	if err != nil {
		return nil, fmt.Errorf("while reading docker archive manifest: %w", err)
	}

	if len(manifest.RepoTags) == 0 {
		return nil, errors.New("no RepoTags found in manifest")
	}

	if manifest.RepoTags[0] == "" {
		return nil, errors.New(
			"unable to add tar archive, manifest does not have a RepoTags entry",
		)
	}

	logrus.Infof("Package describes %s image", manifest.RepoTags[0])

	// Create the new SPDX package
	imagePackage = NewPackage()
	imagePackage.Options().WorkDir = tarOpts.ExtractDir
	imagePackage.Name = manifest.RepoTags[0]
	imagePackage.BuildID(imagePackage.Name)

	logrus.Infof("Image manifest lists %d layers", len(manifest.LayerFiles))

	// Scan the container layers for OS information:
	ct := osinfo.ContainerScanner{}
	var osPackageData *[]osinfo.PackageDBEntry
	var layerNum int
	layerPaths := []string{}
	for _, layerFile := range manifest.LayerFiles {
		layerPaths = append(layerPaths, filepath.Join(tarOpts.ExtractDir, layerFile))
	}

	// Scan for package data if option is set
	if spdxOpts.ScanImages {
		layerNum, osPackageData, err = ct.ReadOSPackages(layerPaths)
		if err != nil {
			return nil, fmt.Errorf("getting os data from container: %w", err)
		}
	}

	if osPackageData != nil {
		logrus.Infof(
			"Scan of container image returned %d OS packages in layer #%d",
			len(*osPackageData), layerNum,
		)
	}

	// Cycle all the layers from the manifest and add them as packages
	for i, layerFile := range manifest.LayerFiles {
		// Generate a package from a layer
		pkg, err := di.PackageFromTarball(spdxOpts, tarOpts, filepath.Join(tarOpts.ExtractDir, layerFile))
		if err != nil {
			return nil, fmt.Errorf("building package from layer: %w", err)
		}

		// Regenerate the BuildID to avoid clashes when handling multiple
		// images at the same time.
		pkg.BuildID(manifest.RepoTags[0], layerFile)

		// If the option is enabled, scan the container layers
		if spdxOpts.AnalyzeLayers {
			if err := di.AnalyzeImageLayer(filepath.Join(tarOpts.ExtractDir, layerFile), pkg); err != nil {
				return nil, fmt.Errorf("scanning layer "+pkg.ID+" :%w", err)
			}
		} else {
			logrus.Info("Not performing deep image analysis (opts.AnalyzeLayers = false)")
		}

		// If we got the OS data from the scanner, add the packages:
		if i == layerNum && osPackageData != nil {
			for i := range *osPackageData {
				ospk := NewPackage()
				ospk.Name = (*osPackageData)[i].Package
				ospk.Version = (*osPackageData)[i].Version
				ospk.HomePage = (*osPackageData)[i].HomePage
				if (*osPackageData)[i].MaintainerName != "" {
					ospk.Supplier.Person = (*osPackageData)[i].MaintainerName
					if (*osPackageData)[i].MaintainerEmail != "" {
						ospk.Supplier.Person += fmt.Sprintf(" (%s)", (*osPackageData)[i].MaintainerEmail)
					}
				}
				if (*osPackageData)[i].PackageURL() != "" {
					ospk.ExternalRefs = append(ospk.ExternalRefs, ExternalRef{
						Category: "PACKAGE-MANAGER",
						Type:     "purl",
						Locator:  (*osPackageData)[i].PackageURL(),
					})
				}
				ospk.BuildID(pkg.ID)
				if err := pkg.AddPackage(ospk); err != nil {
					return nil, fmt.Errorf("adding OS package to container layer: %w", err)
				}
			}
		}

		// Add the layer package to the image package
		if err := imagePackage.AddPackage(pkg); err != nil {
			return nil, fmt.Errorf("adding layer to image package: %w", err)
		}
	}

	// return the finished package
	return imagePackage, nil
}

func (di *spdxDefaultImplementation) AnalyzeImageLayer(layerPath string, pkg *Package) error {
	return NewImageAnalyzer().AnalyzeLayer(layerPath, pkg)
}

// PackageFromDirectory scans a directory and returns its contents as a
// SPDX package, optionally determining the licenses found
func (di *spdxDefaultImplementation) PackageFromDirectory(opts *Options, dirPath string) (pkg *Package, err error) {
	dirPath, err = filepath.Abs(dirPath)
	if err != nil {
		return nil, fmt.Errorf("getting absolute directory path: %w", err)
	}
	fileList, err := di.GetDirectoryTree(dirPath)
	if err != nil {
		return nil, fmt.Errorf("building directory tree: %w", err)
	}
	reader, err := di.LicenseReader(opts)
	if err != nil {
		return nil, fmt.Errorf("creating license reader: %w", err)
	}
	licenseTag := ""
	lic, err := di.GetDirectoryLicense(reader, dirPath, opts)
	if err != nil {
		return nil, fmt.Errorf("scanning directory for licenses: %w", err)
	}
	if lic != nil {
		licenseTag = lic.LicenseID
	}

	// Build a list of patterns from those found in the .gitignore file and
	// posssibly others passed in the options:
	patterns, err := di.IgnorePatterns(
		dirPath, opts.IgnorePatterns, opts.NoGitignore,
	)
	if err != nil {
		return nil, fmt.Errorf("building ignore patterns list: %w", err)
	}

	// Apply the ignore patterns to the list of files
	fileList = di.ApplyIgnorePatterns(fileList, patterns)
	if len(fileList) == 0 {
		return nil, fmt.Errorf("directory %s has no files to scan", dirPath)
	}
	logrus.Infof("Scanning %d files and adding them to the SPDX package", len(fileList))

	pkg = NewPackage()
	pkg.FilesAnalyzed = true
	pkg.Name = filepath.Base(dirPath)
	if pkg.Name == "" {
		pkg.Name = uuid.NewString()
	}
	pkg.LicenseConcluded = licenseTag

	// Set the working directory of the package:
	pkg.Options().WorkDir = filepath.Dir(dirPath)

	t := throttler.New(5, len(fileList))

	processDirectoryFile := func(path string, pkg *Package) {
		defer t.Done(err)
		f := NewFile()
		f.Options().WorkDir = dirPath
		f.Options().Prefix = pkg.Name

		lic, err = reader.LicenseFromFile(filepath.Join(dirPath, path))
		if err != nil {
			err = fmt.Errorf("scanning file for license: %w", err)
			return
		}
		f.LicenseInfoInFile = NONE
		if lic == nil {
			f.LicenseConcluded = licenseTag
		} else {
			f.LicenseInfoInFile = lic.LicenseID
		}

		if err = f.ReadSourceFile(filepath.Join(dirPath, path)); err != nil {
			err = fmt.Errorf("checksumming file: %w", err)
			return
		}
		if err = pkg.AddFile(f); err != nil {
			err = fmt.Errorf("adding %s as file to the spdx package: %w", path, err)
			return
		}
	}

	// Read the files in parallel
	for _, path := range fileList {
		go processDirectoryFile(path, pkg)
		t.Throttle()
	}

	// If the throttler picked an error, fail here
	if err := t.Err(); err != nil {
		return nil, err
	}

	// Add files into the package
	return pkg, nil
}
