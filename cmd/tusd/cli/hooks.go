package cli

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/tus/tusd"

	"github.com/sethgrid/pester"
)

type HookType string

const (
	HookPostFinish    HookType = "post-finish"
	HookPostTerminate HookType = "post-terminate"
	HookPostReceive   HookType = "post-receive"
	HookPostCreate    HookType = "post-create"
	HookPreCreate     HookType = "pre-create"
)

type hookDataStore struct {
	tusd.DataStore
}

var secret []byte

func init() {
	s := os.Getenv("SIG_SECRET")
	if s == "" {
		secret = []byte("!secret!12345678")
	} else {
		secret = []byte(s)
	}
}

func (store hookDataStore) NewUpload(info tusd.FileInfo) (id string, err error) {
	//fmt.Printf("secret: %s\n",string(secret))
	//b, _ := json.Marshal(info)
	//fmt.Println(string(b))
	
	sig := info.MetaData["sig"]
	key := info.MetaData["key"]
	user := info.MetaData["user"]
	name := info.MetaData["name"]
	typ := info.MetaData["type"]
	
	if sig == "" || key == "" || user == "" || name == "" || typ == "" {
		return "", fmt.Errorf("pre-create hook failed: Meta fields sig, key, user, name, type must be provided.\n")
	}
	
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return "", fmt.Errorf("pre-create hook failed: Invalid sig.\n")
	}
	
	keyBytes, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return "", fmt.Errorf("pre-create hook failed: Invalid key.\n")
	}
	
	bsize := make([]byte, 8)
	binary.LittleEndian.PutUint64(bsize, uint64(info.Size))
	
	var buf bytes.Buffer
	buf.Write(keyBytes)
	buf.WriteString(user)
	buf.WriteString(name)
	buf.Write(bsize)
	
	message := buf.Bytes()
	
	hash := hmac.New(sha256.New, secret)
	hash.Write(message)
	
	signed := hash.Sum(nil)
	
	if !bytes.Equal(sigBytes, signed) {
		return "", fmt.Errorf("pre-create hook failed: Unauthorized.\n")
	}
	
	if output, err := invokeHookSync(HookPreCreate, info, true); err != nil {
		return "", fmt.Errorf("pre-create hook failed: %s\n%s", err, string(output))
	}
	return store.DataStore.NewUpload(info)
}

func SetupPreHooks(composer *tusd.StoreComposer) {
	composer.UseCore(hookDataStore{
		DataStore: composer.Core,
	})
}

func SetupPostHooks(handler *tusd.Handler) {
	go func() {
		for {
			select {
			case info := <-handler.CompleteUploads:
				invokeHook(HookPostFinish, info)
			case info := <-handler.TerminatedUploads:
				invokeHook(HookPostTerminate, info)
			case info := <-handler.UploadProgress:
				invokeHook(HookPostReceive, info)
			case info := <-handler.CreatedUploads:
				invokeHook(HookPostCreate, info)
			}
		}
	}()
}

func invokeHook(typ HookType, info tusd.FileInfo) {
	go func() {
		// Error handling is token care of by the function.
		_, _ = invokeHookSync(typ, info, false)
	}()
}

func invokeHookSync(typ HookType, info tusd.FileInfo, captureOutput bool) ([]byte, error) {
	switch typ {
	case HookPostFinish:
		logEv("UploadFinished", "id", info.ID, "size", strconv.FormatInt(info.Size, 10))
	case HookPostTerminate:
		logEv("UploadTerminated", "id", info.ID)
	}

	if !Flags.FileHooksInstalled && !Flags.HttpHooksInstalled {
		return nil, nil
	}
	name := string(typ)
	logEv("HookInvocationStart", "type", name, "id", info.ID)

	output := []byte{}
	err := error(nil)

	if Flags.FileHooksInstalled {
		output, err = invokeFileHook(name, typ, info, captureOutput)
	}

	if Flags.HttpHooksInstalled {
		output, err = invokeHttpHook(name, typ, info, captureOutput)
	}

	if err != nil {
		logEv("HookInvocationError", "type", string(typ), "id", info.ID, "error", err.Error())
	} else {
		logEv("HookInvocationFinish", "type", string(typ), "id", info.ID)
	}

	return output, err
}

func invokeHttpHook(name string, typ HookType, info tusd.FileInfo, captureOutput bool) ([]byte, error) {
	jsonInfo, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", Flags.HttpHooksEndpoint, bytes.NewBuffer(jsonInfo))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Hook-Name", name)
	req.Header.Set("Content-Type", "application/json")

	// Use linear backoff strategy with the user defined values.
	client := pester.New()
	client.KeepLog = true
	client.MaxRetries = Flags.HttpHooksRetry
	client.Backoff = func(_ int) time.Duration {
		return time.Duration(Flags.HttpHooksBackoff) * time.Second
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return body, fmt.Errorf("endpoint returned: %s\n%s", resp.Status, body)
	}

	if captureOutput {
		return body, err
	}

	return nil, err
}

func invokeFileHook(name string, typ HookType, info tusd.FileInfo, captureOutput bool) ([]byte, error) {
	cmd := exec.Command(Flags.FileHooksDir + "/" + name)
	env := os.Environ()
	env = append(env, "TUS_ID="+info.ID)
	env = append(env, "TUS_SIZE="+strconv.FormatInt(info.Size, 10))
	env = append(env, "TUS_OFFSET="+strconv.FormatInt(info.Offset, 10))

	jsonInfo, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(jsonInfo)
	cmd.Stdin = reader

	cmd.Env = env
	cmd.Dir = Flags.FileHooksDir
	cmd.Stderr = os.Stderr

	// If `captureOutput` is true, this function will return the output (both,
	// stderr and stdout), else it will use this process' stdout
	var output []byte
	if !captureOutput {
		cmd.Stdout = os.Stdout
		err = cmd.Run()
	} else {
		output, err = cmd.Output()
	}

	// Ignore the error, only, if the hook's file could not be found. This usually
	// means that the user is only using a subset of the available hooks.
	if os.IsNotExist(err) {
		err = nil
	}

	return output, err
}
