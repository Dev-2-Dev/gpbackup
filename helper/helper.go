package helper

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/greenplum-db/gp-common-go-libs/gplog"
	"github.com/greenplum-db/gp-common-go-libs/iohelper"
	"github.com/greenplum-db/gp-common-go-libs/operating"
	"github.com/greenplum-db/gpbackup/utils"
	yaml "gopkg.in/yaml.v2"
)

/*
 * Non-flag variables
 */

var (
	CleanupGroup  *sync.WaitGroup
	currentPipe   string
	errBuf        bytes.Buffer
	lastPipe      string
	nextPipe      string
	version       string
	wasTerminated bool
	closeMutex    *sync.Mutex
	//writeHandle   *os.File
	//writer        *bufio.Writer
)

/*
 * Command-line flags
 */
var (
	backupAgent      *bool
	compressionLevel *int
	content          *int
	dataFile         *string
	oidFile          *string
	pipeFile         *string
	pluginConfigFile *string
	printVersion     *bool
	restoreAgent     *bool
	tocFile          *string
)

func DoHelper() {
	defer DoTeardown()
	InitializeGlobals()
	utils.InitializeSignalHandler(DoCleanup, fmt.Sprintf("helper agent on segment %d", *content), &wasTerminated)
	if *backupAgent {
		doBackupAgent()
	} else if *restoreAgent {
		doRestoreAgent()
	}
}

func InitializeGlobals() {
	CleanupGroup = &sync.WaitGroup{}
	CleanupGroup.Add(1)
	gplog.InitializeLogging("gpbackup_helper", "")

	backupAgent = flag.Bool("backup-agent", false, "Use gpbackup_helper as an agent for backup")
	content = flag.Int("content", -2, "Content ID of the corresponding segment")
	compressionLevel = flag.Int("compression-level", 0, "The level of compression to use with gzip. O indicates no compression.")
	dataFile = flag.String("data-file", "", "Absolute path to the data file")
	oidFile = flag.String("oid-file", "", "Absolute path to the file containing a list of oids to restore")
	pipeFile = flag.String("pipe-file", "", "Absolute path to the pipe file")
	pluginConfigFile = flag.String("plugin-config", "", "The configuration file to use for a plugin")
	printVersion = flag.Bool("version", false, "Print version number and exit")
	restoreAgent = flag.Bool("restore-agent", false, "Use gpbackup_helper as an agent for restore")
	tocFile = flag.String("toc-file", "", "Absolute path to the table of contents file")

	flag.Parse()
	if *printVersion {
		fmt.Printf("gpbackup_helper version %s\n", version)
		os.Exit(0)
	}
	operating.InitializeSystemFunctions()
}

/*
 * Backup specific functions
 */

func doBackupAgent() {
	var lastRead uint64
	var (
		finalWriter io.Writer
		gzipWriter  *gzip.Writer
		bufIoWriter *bufio.Writer
		writeHandle io.WriteCloser
		writeCmd    *exec.Cmd
	)
	toc := &utils.SegmentTOC{}
	toc.DataEntries = make(map[uint]utils.SegmentDataEntry, 0)

	oidList := getOidListFromFile()[0]

	currentPipe = fmt.Sprintf("%s_%d", *pipeFile, oidList[0])
	/*
	 * It is important that we create the reader before creating the writer
	 * so that we establish a connection to the first pipe (created by gpbackup)
	 * and properly clean it up if an error occurs while creating the writer.
	 */
	for i, oid := range oidList {
		if wasTerminated {
			return
		}
		if i < len(oidList)-1 {
			log(fmt.Sprintf("Creating pipe for oid %d\n", oidList[i+1]))
			nextPipe = fmt.Sprintf("%s_%d", *pipeFile, oidList[i+1])
			createPipe(nextPipe)
		}

		log(fmt.Sprintf("Opening pipe for oid %d\n", oid))
		reader, readHandle := getBackupPipeReader(currentPipe)
		if i == 0 {
			finalWriter, gzipWriter, bufIoWriter, writeHandle, writeCmd = getBackupPipeWriter(*compressionLevel)
		}

		log(fmt.Sprintf("Backing up table with oid %d\n", oid))
		numBytes, err := io.Copy(finalWriter, reader)
		gplog.FatalOnError(err, strings.Trim(errBuf.String(), "\x00"))
		log(fmt.Sprintf("Read %d bytes\n", numBytes))

		lastProcessed := lastRead + uint64(numBytes)
		toc.AddSegmentDataEntry(uint(oid), lastRead, lastProcessed)
		lastRead = lastProcessed

		lastPipe = currentPipe
		currentPipe = nextPipe
		_ = readHandle.Close()
		removeFileIfExists(lastPipe)
	}

	/*
	 * The order for flushing and closing the writers below is very specific
	 * to ensure all data is written to the file and file handles are not leaked.
	 */
	if gzipWriter != nil {
		_ = gzipWriter.Close()
	}
	_ = bufIoWriter.Flush()
	_ = writeHandle.Close()
	if *pluginConfigFile != "" {
		/*
		 * When using a plugin, the agent may take longer to finish than the
		 * main gpbackup process. We either write the TOC file if the agent finishes
		 * successfully or write an error file if it has an error after the COPYs have
		 * finished. We then wait on the gpbackup side until one of those files is
		 * written to verify the agent completed.
		 */
		log("Uploading remaining data to plugin destination")
		if err := writeCmd.Wait(); err != nil {
			handle := iohelper.MustOpenFileForWriting(fmt.Sprintf("%s_error", *pipeFile))
			_ = handle.Close()
			gplog.Fatal(err, strings.Trim(errBuf.String(), "\x00"))
		}
	}
	toc.WriteToFileAndMakeReadOnly(*tocFile)
	log("Finished writing segment TOC")
}

func getBackupPipeReader(currentPipe string) (io.Reader, io.ReadCloser) {
	readHandle, err := os.OpenFile(currentPipe, os.O_RDONLY, os.ModeNamedPipe)
	gplog.FatalOnError(err)
	// This is a workaround for https://github.com/golang/go/issues/24164.
	// Once this bug is fixed, the call to Fd() can be removed
	readHandle.Fd()
	reader := bufio.NewReader(readHandle)
	return reader, readHandle
}

func getBackupPipeWriter(compressLevel int) (io.Writer, *gzip.Writer, *bufio.Writer, io.WriteCloser, *exec.Cmd) {
	var writeHandle io.WriteCloser
	var err error
	var writeCmd *exec.Cmd
	if *pluginConfigFile != "" {
		writeCmd, writeHandle = startBackupPluginCommand()
	} else {
		writeHandle, err = os.Create(*dataFile)
		gplog.FatalOnError(err)
	}

	var finalWriter io.Writer
	var gzipWriter *gzip.Writer
	bufIoWriter := bufio.NewWriter(writeHandle)
	finalWriter = bufIoWriter
	if compressLevel > 0 {
		gzipWriter, err = gzip.NewWriterLevel(bufIoWriter, compressLevel)
		gplog.FatalOnError(err)
		finalWriter = gzipWriter
	}
	return finalWriter, gzipWriter, bufIoWriter, writeHandle, writeCmd
}

func startBackupPluginCommand() (*exec.Cmd, io.WriteCloser) {
	pluginConfig := utils.ReadPluginConfig(*pluginConfigFile)
	cmdStr := fmt.Sprintf("%s backup_data %s %s", pluginConfig.ExecutablePath, pluginConfig.ConfigPath, *dataFile)
	writeCmd := exec.Command("bash", "-c", cmdStr)

	writeHandle, err := writeCmd.StdinPipe()
	gplog.FatalOnError(err)
	writeCmd.Stderr = &errBuf
	err = writeCmd.Start()
	gplog.FatalOnError(err)
	return writeCmd, writeHandle
}

/*
 * Restore specific functions
 */

func doRestoreAgent() {
	tocEntries := utils.NewSegmentTOC(*tocFile).DataEntries
	allOids := getOidListFromFile()

	var workerPool sync.WaitGroup
	var fatalErr error
	for _, oidsForConn := range allOids {
		workerPool.Add(1)
		go func(oidList []uint32) {
			var (
				lastByte       uint64
				resLastPipe    string
				resCurrentPipe string
				resNextPipe    string
				resWriteHandle *os.File
				resWriter      *bufio.Writer
				resReader      *bufio.Reader
			)

			defer workerPool.Done()
			resCurrentPipe = fmt.Sprintf("%s_%d", *pipeFile, oidList[0])
			log(fmt.Sprintf("Opening pipe for oid %d", oidList[0]))
			/*
			 * It is important that we create the writer before creating the reader
			 * so that we establish a connection to the first pipe (created by gprestore)
			 * and properly clean it up if an error occurs while creating the reader.
			 */
			resWriter, resWriteHandle = getRestorePipeWriter(resCurrentPipe)
			resReader = getRestorePipeReader()

			defer func() {
				flushAndCloseRestoreWriter(resWriter, resWriteHandle)
				removeFileIfExists(resLastPipe)
				removeFileIfExists(resCurrentPipe)
				removeFileIfExists(resNextPipe)
			}()

			for i, oid := range oidList {
				if wasTerminated || fatalErr != nil {
					return
				}
				log(fmt.Sprintf("Restoring table with oid %d", oid))
				if i < len(oidList)-1 {
					resNextPipe = fmt.Sprintf("%s_%d", *pipeFile, oidList[i+1])
					createPipe(resNextPipe)
				} else {
					resNextPipe = ""
				}
				start := tocEntries[uint(oid)].StartByte
				end := tocEntries[uint(oid)].EndByte
				log(fmt.Sprintf("Start Byte: %d; End Byte: %d; Last Byte: %d", start, end, lastByte))
				_, err := resReader.Discard(int(start - lastByte))
				if err != nil {
					fatalErr = err
				}
				log(fmt.Sprintf("Discarded %d bytes", start-lastByte))
				bytesRead, err := io.CopyN(resWriter, resReader, int64(end-start))
				log(fmt.Sprintf("Read %d bytes", bytesRead))
				if err != nil {
					fatalErr = err
				}
				log(fmt.Sprintf("Closing pipe for oid %d", oid))
				flushAndCloseRestoreWriter(resWriter, resWriteHandle)
				lastByte = end

				resLastPipe = resCurrentPipe
				resCurrentPipe = resNextPipe
				removeFileIfExists(resLastPipe)
				if resCurrentPipe != "" {
					log(fmt.Sprintf("Opening pipe for oid %d", oid))
					resWriter, resWriteHandle = getRestorePipeWriter(resCurrentPipe)
				}
			}
		}(oidsForConn)
	}
	workerPool.Wait()
	gplog.FatalOnError(fatalErr, strings.Trim(errBuf.String(), "\x00"))
}

func getRestorePipeReader() *bufio.Reader {
	var readHandle io.Reader
	var err error
	if *pluginConfigFile != "" {
		readHandle = startRestorePluginCommand()
		defer func() {
			errString := strings.Trim(errBuf.String(), "\x00")
			if len(errString) != 0 {
				gplog.Error(errString)
			}
		}()
	} else {
		readHandle, err = os.Open(*dataFile)
		gplog.FatalOnError(err)
	}

	var bufIoReader *bufio.Reader
	if strings.HasSuffix(*dataFile, ".gz") {
		gzipReader, err := gzip.NewReader(readHandle)
		gplog.FatalOnError(err)
		bufIoReader = bufio.NewReader(gzipReader)
	} else {
		bufIoReader = bufio.NewReader(readHandle)
	}
	return bufIoReader
}

func startRestorePluginCommand() io.Reader {
	pluginConfig := utils.ReadPluginConfig(*pluginConfigFile)
	cmdStr := fmt.Sprintf("%s restore_data %s %s", pluginConfig.ExecutablePath, pluginConfig.ConfigPath, *dataFile)
	cmd := exec.Command("bash", "-c", cmdStr)

	readHandle, err := cmd.StdoutPipe()
	gplog.FatalOnError(err)
	errBuf = *bytes.NewBuffer([]byte{})
	cmd.Stderr = &errBuf

	err = cmd.Start()
	gplog.FatalOnError(err)
	return readHandle

}

func getRestorePipeWriter(currentPipe string) (*bufio.Writer, *os.File) {
	fileHandle, err := os.OpenFile(currentPipe, os.O_WRONLY, os.ModeNamedPipe)
	gplog.FatalOnError(err)
	pipeWriter := bufio.NewWriter(fileHandle)
	return pipeWriter, fileHandle
}

/*
 * Shared functions
 */

func createPipe(pipe string) {
	err := syscall.Mkfifo(pipe, 0777)
	gplog.FatalOnError(err)
}

func getOidListFromFile() map[int][]uint32 {
	oidStr, err := operating.System.ReadFile(*oidFile)
	gplog.FatalOnError(err)
	output := make(map[int][]uint32, 0)
	err = yaml.Unmarshal(oidStr, &output)
	gplog.FatalOnError(err)
	return output
}

func flushAndCloseRestoreWriter(writer *bufio.Writer, writeHandle *os.File) {
	closeMutex.Lock()
	if writer != nil {
		err := writer.Flush()
		gplog.FatalOnError(err)
		writer = nil
	}
	if writeHandle != nil {
		err := writeHandle.Close()
		gplog.FatalOnError(err)
		writeHandle = nil
	}
	closeMutex.Unlock()
}

func fileExists(filename string) bool {
	_, err := operating.System.Stat(filename)
	return err == nil
}

func removeFileIfExists(filename string) {
	if fileExists(filename) {
		err := os.Remove(filename)
		gplog.FatalOnError(err)
	}
}

/*
 * Shared helper functions
 */

func DoTeardown() {
	_ = recover()
	if wasTerminated {
		CleanupGroup.Wait()
		return
	}
	DoCleanup()
	os.Exit(gplog.GetErrorCode())
}

func DoCleanup() {
	defer func() {
		if err := recover(); err != nil {
			log("Encountered error during cleanup: %v", err)
		}
		log("Cleanup complete")
		CleanupGroup.Done()
	}()
	if wasTerminated {
		/*
		 * If the agent dies during the last table copy, it can still report
		 * success, so we create an error file and check for its presence in
		 * gprestore after the COPYs are finished.
		 */
		handle := iohelper.MustOpenFileForWriting(fmt.Sprintf("%s_error", *pipeFile))
		_ = handle.Close()
	}
	//TODO: how will this work with multiple goroutines?
	//flushAndCloseRestoreWriter()
	removeFileIfExists(lastPipe)
	removeFileIfExists(currentPipe)
	removeFileIfExists(nextPipe)
}

func log(s string, v ...interface{}) {
	s = fmt.Sprintf("Segment %d: %s", *content, s)
	gplog.Verbose(s, v...)
}
