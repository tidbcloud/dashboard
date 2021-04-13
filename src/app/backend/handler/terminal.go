// Copyright 2017 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"log"
	"net/http"
	"strings"
	"sync"

	restful "github.com/emicklei/go-restful/v3"
	"gopkg.in/igm/sockjs-go.v2/sockjs"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog"
)

const (
	defaultImage  = "knightxun/tidb-debug:latest"
	launcherImage = "knightxun/debug-launcher:latest"
	launcherName  = "debug-launcher"
	DockerSocket = "/var/run/docker.sock"
)

const END_OF_TRANSMISSION = "\u0004"

// PtyHandler is what remotecommand expects from a pty
type PtyHandler interface {
	io.Reader
	io.Writer
	remotecommand.TerminalSizeQueue
}

// TerminalSession implements PtyHandler (using a SockJS connection)
type TerminalSession struct {
	id            string
	PodName       string
	Namespace     string
	bound         chan error
	sockJSSession sockjs.Session
	sizeChan      chan remotecommand.TerminalSize
	doneChan      chan struct{}
}

// TerminalMessage is the messaging protocol between ShellController and TerminalSession.
//
// OP      DIRECTION  FIELD(S) USED  DESCRIPTION
// ---------------------------------------------------------------------
// bind    fe->be     SessionID      Id sent back from TerminalResponse
// stdin   fe->be     Data           Keystrokes/paste buffer
// resize  fe->be     Rows, Cols     New terminal size
// stdout  be->fe     Data           Output from the process
// toast   be->fe     Data           OOB message to be shown to the user
type TerminalMessage struct {
	Op, Data, SessionID string
	Rows, Cols          uint16
}

// TerminalSize handles pty->process resize events
// Called in a loop from remotecommand as long as the process is running
func (t TerminalSession) Next() *remotecommand.TerminalSize {
	select {
	case size := <-t.sizeChan:
		return &size
	case <-t.doneChan:
		return nil
	}
}

// Read handles pty->process messages (stdin, resize)
// Called in a loop from remotecommand as long as the process is running
func (t TerminalSession) Read(p []byte) (int, error) {
	m, err := t.sockJSSession.Recv()
	if err != nil {
		// Send terminated signal to process to avoid resource leak
		return copy(p, END_OF_TRANSMISSION), err
	}

	var msg TerminalMessage
	if err := json.Unmarshal([]byte(m), &msg); err != nil {
		return copy(p, END_OF_TRANSMISSION), err
	}

	klog.Info("")
	switch msg.Op {
	case "stdin":
		return copy(p, msg.Data), nil
	case "resize":
		t.sizeChan <- remotecommand.TerminalSize{Width: msg.Cols, Height: msg.Rows}
		return 0, nil
	default:
		return copy(p, END_OF_TRANSMISSION), fmt.Errorf("unknown message type '%s'", msg.Op)
	}
}

// Write handles process->pty stdout
// Called from remotecommand whenever there is any output
func (t TerminalSession) Write(p []byte) (int, error) {
	msg, err := json.Marshal(TerminalMessage{
		Op:   "stdout",
		Data: string(p),
	})
	if err != nil {
		return 0, err
	}

	if err = t.sockJSSession.Send(string(msg)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Toast can be used to send the user any OOB messages
// hterm puts these in the center of the terminal
func (t TerminalSession) Toast(p string) error {
	msg, err := json.Marshal(TerminalMessage{
		Op:   "toast",
		Data: p,
	})
	if err != nil {
		return err
	}

	if err = t.sockJSSession.Send(string(msg)); err != nil {
		return err
	}
	return nil
}

// SessionMap stores a map of all TerminalSession objects and a lock to avoid concurrent conflict
type SessionMap struct {
	Sessions map[string]TerminalSession
	Lock     sync.RWMutex
}

// Get return a given terminalSession by sessionId
func (sm *SessionMap) Get(sessionId string) TerminalSession {
	sm.Lock.RLock()
	defer sm.Lock.RUnlock()
	return sm.Sessions[sessionId]
}

// Set store a TerminalSession to SessionMap
func (sm *SessionMap) Set(sessionId string, session TerminalSession) {
	sm.Lock.Lock()
	defer sm.Lock.Unlock()
	sm.Sessions[sessionId] = session
}

// Close shuts down the SockJS connection and sends the status code and reason to the client
// Can happen if the process exits or if there is an error starting up the process
// For now the status code is unused and reason is shown to the user (unless "")
func (sm *SessionMap) Close(k8sClient kubernetes.Interface, sessionId string, status uint32, reason string) {
	sm.Lock.Lock()
	defer sm.Lock.Unlock()

	err := sm.Sessions[sessionId].sockJSSession.Close(status, reason)
	if err != nil {
		log.Println(err)
	}

	delete(sm.Sessions, sessionId)
}

var terminalSessions = SessionMap{Sessions: make(map[string]TerminalSession)}

// handleTerminalSession is Called by net/http for any new /api/sockjs connections
func handleTerminalSession(session sockjs.Session) {
	var (
		buf             string
		err             error
		msg             TerminalMessage
		terminalSession TerminalSession
	)

	if buf, err = session.Recv(); err != nil {
		log.Printf("handleTerminalSession: can't Recv: %v", err)
		return
	}

	if err = json.Unmarshal([]byte(buf), &msg); err != nil {
		log.Printf("handleTerminalSession: can't UnMarshal (%v): %s", err, buf)
		return
	}

	if msg.Op != "bind" {
		log.Printf("handleTerminalSession: expected 'bind' message, got: %s", buf)
		return
	}

	if terminalSession = terminalSessions.Get(msg.SessionID); terminalSession.id == "" {
		log.Printf("handleTerminalSession: can't find session '%s'", msg.SessionID)
		return
	}

	terminalSession.sockJSSession = session
	terminalSessions.Set(msg.SessionID, terminalSession)
	terminalSession.bound <- nil
}

// CreateAttachHandler is called from main for /api/sockjs
func CreateAttachHandler(path string) http.Handler {
	return sockjs.NewHandler(path, sockjs.DefaultOptions, handleTerminalSession)
}

// startProcess is called by handleAttach
// Executed cmd in the container specified in request and connects it up with the ptyHandler (a session)
//func startProcess(k8sClient kubernetes.Interface, cfg *rest.Config, request *restful.Request, cmd []string, sessionId string) error {
//	fmt.Println("Start Exec Process")
//	ptyHandler := terminalSessions.Get(sessionId)
//	namespace := request.PathParameter("namespace")
//	podName := request.PathParameter("pod")
//	containerName := request.PathParameter("container")
//	debugPodName := fmt.Sprintf("%s-%s-%s-%s", namespace, podName, containerName, launcherName)
//
//	cmd = []string{"/debug-launcher","--target-container",
//		"docker://2ecaef8b5aa5bcc9e1ddc04d99643f7a013c97b619068cdbeaa5045c156ffe83",
//		"--image", "knightxun/tidb-debug:latest", "--docker-socket",
//		"unix:///var/run/docker.sock", "--", "bash", "-l"}
//
//	if strings.Contains(podName, containerName) && strings.Contains(podName, namespace) &&
//		strings.Contains(podName, launcherName) {
//
//		req := k8sClient.CoreV1().RESTClient().Post().
//			Resource("pods").
//			Name(podName).
//			Namespace("default").
//			SubResource("exec")
//
//		req.VersionedParams(&v1.PodExecOptions{
//			Container: "debug-launcher",
//			Command:   cmd,
//			Stdin:     true,
//			Stdout:    true,
//			Stderr:    true,
//			TTY:       true,
//		}, scheme.ParameterCodec)
//
//		exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
//		if err != nil {
//			return err
//		}
//
//		err = exec.Stream(remotecommand.StreamOptions{
//			Stdin:             ptyHandler,
//			Stdout:            ptyHandler,
//			Stderr:            ptyHandler,
//			TerminalSizeQueue: ptyHandler,
//			Tty:               true,
//		})
//		if err != nil {
//			return err
//		}
//
//		return nil
//	}
//
//	ptyHandler.PodName = debugPodName
//	ptyHandler.Namespace = namespace
//
//	terminalSessions.Set(sessionId, ptyHandler)
//
//	_, err := k8sClient.CoreV1().Pods("default").Get(context.Background(), debugPodName, metav1.GetOptions{})
//
//	if errors.IsNotFound(err) {
//		execOptions := PodExecOptions{
//			KubeCli:          k8sClient,
//			Image:            defaultImage,
//			HostDockerSocket: DockerSocket,
//			LauncherImage:    launcherImage,
//			PodName: 					podName,
//			Namespace:        namespace,
//			ContainerName:    containerName,
//			Command:          []string{"bash", "-l"},
//			IOStreams: genericclioptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr},
//		}
//		err := execOptions.Run()
//		if err != nil {
//			return err
//		}
//	}
//
//	req := k8sClient.CoreV1().RESTClient().Post().
//		Resource("pods").
//		Name(debugPodName).
//		Namespace(namespace).
//		SubResource("exec")
//
//	req.VersionedParams(&v1.PodExecOptions{
//		Container: containerName,
//		Command:   cmd,
//		Stdin:     true,
//		Stdout:    true,
//		Stderr:    true,
//		TTY:       true,
//	}, scheme.ParameterCodec)
//
//	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
//	if err != nil {
//		return err
//	}
//
//	err = exec.Stream(remotecommand.StreamOptions{
//		Stdin:             ptyHandler,
//		Stdout:            ptyHandler,
//		Stderr:            ptyHandler,
//		TerminalSizeQueue: ptyHandler,
//		Tty:               true,
//	})
//	if err != nil {
//		return err
//	}
//
//	return nil
//}


func startProcess(k8sClient kubernetes.Interface, cfg *rest.Config, request *restful.Request, cmd []string, sessionId string) error {
	fmt.Println("Start Exec Process")
	ptyHandler := terminalSessions.Get(sessionId)
	namespace := request.PathParameter("namespace")
	podName := request.PathParameter("pod")
	containerName := request.PathParameter("container")

	nodeName, containerID, err := getPodInfo(k8sClient, namespace, podName, containerName)

	debugPodName := fmt.Sprintf("default-%s-%s", nodeName, launcherName)

	if podName == debugPodName && namespace == "default" {
		return fmt.Errorf("Can't debug debug-pod")
	}

	err = createDebugPod(k8sClient, nodeName, debugPodName)
	if err != nil {
		return err
	}

	cmd = []string{"/debug-launcher","--target-container",
		containerID,
		"--image", defaultImage, "--docker-socket",
		"unix:///var/run/docker.sock"}

	fmt.Println("exec Command: ", strings.Join(cmd, " "))
	req := k8sClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(debugPodName).
		Namespace("default").
		SubResource("exec")

	req.VersionedParams(&v1.PodExecOptions{
		Container: containerName,
		Command:   []string{"/bin/bash", "-c", strings.Join(cmd, " ")},
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		fmt.Println("Exec Into Pod Failed: %v", err)
		return err
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:             ptyHandler,
		Stdout:            ptyHandler,
		Stderr:            ptyHandler,
		TerminalSizeQueue: ptyHandler,
		Tty:               true,
	})
	if err != nil {
		return err
	}

	return nil
}


// genTerminalSessionId generates a random session ID string. The format is not really interesting.
// This ID is used to identify the session when the client opens the SockJS connection.
// Not the same as the SockJS session id! We can't use that as that is generated
// on the client side and we don't have it yet at this point.
func genTerminalSessionId() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	id := make([]byte, hex.EncodedLen(len(bytes)))
	hex.Encode(id, bytes)
	return string(id), nil
}

// isValidShell checks if the shell is an allowed one
func isValidShell(validShells []string, shell string) bool {
	for _, validShell := range validShells {
		if validShell == shell {
			return true
		}
	}
	return false
}

// WaitForTerminal is called from apihandler.handleAttach as a goroutine
// Waits for the SockJS connection to be opened by the client the session to be bound in handleTerminalSession
func WaitForTerminal(k8sClient kubernetes.Interface, cfg *rest.Config, request *restful.Request, sessionId string) {
	shell := request.QueryParameter("shell")
	fmt.Println("Wait For Terminal")

	select {
	case <-terminalSessions.Get(sessionId).bound:
		close(terminalSessions.Get(sessionId).bound)

		var err error
		validShells := []string{"bash", "sh", "powershell", "cmd"}

		if isValidShell(validShells, shell) {
			cmd := []string{shell}
			err = startProcess(k8sClient, cfg, request, cmd, sessionId)
		} else {
			// No shell given or it was not valid: try some shells until one succeeds or all fail
			// FIXME: if the first shell fails then the first keyboard event is lost
			for _, testShell := range validShells {
				cmd := []string{testShell}
				if err = startProcess(k8sClient, cfg, request, cmd, sessionId); err == nil {
					break
				}
			}
		}

		if err != nil {
			terminalSessions.Close(k8sClient, sessionId, 2, err.Error())
			return
		}

		terminalSessions.Close(k8sClient, sessionId, 1, "Process exited")
	}
}

type PodExecOptions struct {
	Namespace string
	PodName   string

	// Debug options
	Image            string
	ContainerName    string
	Command          []string
	HostDockerSocket string
	LauncherImage    string

	KubeCli kubernetes.Interface

	RestConfig *rest.Config

	genericclioptions.IOStreams
}

func getPodInfo(kubeCli kubernetes.Interface, namespace string, podName string, containerName string) (string, string, error) {
	pod, err := kubeCli.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		return "", "", fmt.Errorf("cannot debug in a completed pod; current phase is %s", pod.Status.Phase)
	}

	if len(containerName) == 0 {
		containerName = pod.Spec.Containers[0].Name
	}

	nodeName := pod.Spec.NodeName
	targetContainerID, err := getContainerIDByName(pod, containerName)
	if err != nil {
		return "", "", err
	}

	return nodeName, targetContainerID, nil
}

func getContainerIDByName(pod *v1.Pod, containerName string) (string, error) {
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name != containerName {
			continue
		}
		if !containerStatus.Ready {
			return "", fmt.Errorf("container [%s] not ready", containerName)
		}
		return containerStatus.ContainerID, nil
	}

	// also search init containers
	for _, initContainerStatus := range pod.Status.InitContainerStatuses {
		if initContainerStatus.Name != containerName {
			continue
		}
		if initContainerStatus.State.Running == nil {
			return "", fmt.Errorf("init container [%s] is not running", containerName)
		}
		return initContainerStatus.ContainerID, nil
	}

	return "", fmt.Errorf("cannot find specified container %s", containerName)
}

func createDebugPod(kubeCli kubernetes.Interface, nodeName string, podName string) error {
	// 0.Prepare debug context: get Pod and verify state
	_, err := kubeCli.CoreV1().Pods("default").Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}

	launcher := makeLauncherPod(podName, nodeName)
	_, err = kubeCli.CoreV1().Pods(launcher.Namespace).Create(context.Background(), launcher, metav1.CreateOptions{})

	if err != nil {
		if !errors.IsAlreadyExists(err) {
			fmt.Println("Create Debug Pod Failed!")
			return err
		}
	}

	return nil
}

func makeLauncherPod(podName string,  nodeName string) *v1.Pod {
	volume, mount := MakeDockerSocketMount(true)
	// we always mount docker socket to default path despite the host docker socket path
	//launchArgs := []string{
	//	"/bin/bash",
	//	"sleep",
	//	"100000000000000",
	//}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            launcherName,
					Image:           launcherImage,
					Command: 				[]string{ "/bin/bash", "-c", "--" },
					Args: 					[]string{ "while true; do sleep 30; done;" },
					Stdin:           true,
					TTY:             true,
					VolumeMounts:    []v1.VolumeMount{mount},
					ImagePullPolicy: v1.PullAlways,
				},
			},
			Volumes:       []v1.Volume{volume},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
}

//func (o *PodExecOptions) Run() error {
//
//	// 0.Prepare debug context: get Pod and verify state
//	pod, err := o.KubeCli.CoreV1().Pods(o.Namespace).Get(context.Background(), o.PodName, metav1.GetOptions{})
//	if err != nil {
//		return err
//	}
//	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
//		return fmt.Errorf("cannot debug in a completed pod; current phase is %s", pod.Status.Phase)
//	}
//
//	containerName := o.ContainerName
//	if len(containerName) == 0 {
//		containerName = pod.Spec.Containers[0].Name
//	}
//
//	nodeName := pod.Spec.NodeName
//	targetContainerID, err := o.getContainerIDByName(pod, containerName)
//	if err != nil {
//		return err
//	}
//
//	launcher := o.makeLauncherPod(nodeName, targetContainerID, o.Command)
//	pod, err = o.KubeCli.CoreV1().Pods(launcher.Namespace).Create(context.Background(), launcher, metav1.CreateOptions{})
//
//	if err != nil {
//		fmt.Println("Ceate Debug Pod Failed!")
//		return err
//	}
//
//	return nil
//}
//func (o *PodExecOptions) makeLauncherPod(nodeName, containerID string, command []string) *v1.Pod {
//	volume, mount := MakeDockerSocketMount(true)
//	// we always mount docker socket to default path despite the host docker socket path
//	launchArgs := []string{
//		"--target-container",
//		containerID,
//		"--image",
//		o.Image,
//		"--docker-socket",
//		fmt.Sprintf("unix://%s", DockerSocket),
//	}
//
//	launchArgs = append(launchArgs, "--")
//	launchArgs = append(launchArgs, command...)
//	return &v1.Pod{
//		ObjectMeta: metav1.ObjectMeta{
//			Name:      fmt.Sprintf("%s-%s-%s-%s", o.Namespace, o.PodName, o.ContainerName, launcherName),
//			Namespace: o.Namespace,
//		},
//		Spec: v1.PodSpec{
//			Containers: []v1.Container{
//				{
//					Name:            launcherName,
//					Image:           o.LauncherImage,
//					Args:            launchArgs,
//					Stdin:           true,
//					TTY:             true,
//					VolumeMounts:    []v1.VolumeMount{mount},
//					ImagePullPolicy: v1.PullAlways,
//				},
//			},
//			Volumes:       []v1.Volume{volume},
//			NodeName:      nodeName,
//			RestartPolicy: v1.RestartPolicyNever,
//		},
//	}
//}
//
//func (o *PodExecOptions) getContainerIDByName(pod *v1.Pod, containerName string) (string, error) {
//	for _, containerStatus := range pod.Status.ContainerStatuses {
//		if containerStatus.Name != containerName {
//			continue
//		}
//		if !containerStatus.Ready {
//			return "", fmt.Errorf("container [%s] not ready", containerName)
//		}
//		return containerStatus.ContainerID, nil
//	}
//
//	// also search init containers
//	for _, initContainerStatus := range pod.Status.InitContainerStatuses {
//		if initContainerStatus.Name != containerName {
//			continue
//		}
//		if initContainerStatus.State.Running == nil {
//			return "", fmt.Errorf("init container [%s] is not running", containerName)
//		}
//		return initContainerStatus.ContainerID, nil
//	}
//
//	return "", fmt.Errorf("cannot find specified container %s", containerName)
//}

func MakeDockerSocketMount(readOnly bool) (volume v1.Volume, mount v1.VolumeMount) {
	hostSocket := v1.HostPathSocket
	mount = v1.VolumeMount{
		Name:      "docker",
		ReadOnly:  readOnly,
		MountPath: "/var/run/docker.sock",
	}
	volume = v1.Volume{
		Name: "docker",
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: "/var/run/docker.sock",
				Type: &hostSocket,
			},
		},
	}
	return
}
