// Package operator contains main implementation of Flatcar Linux Update Operator.
package operator

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/coreos/locksmith/pkg/timeutil"
	"github.com/kinvolk/flatcar-linux-update-operator/pkg/constants"
	"github.com/kinvolk/flatcar-linux-update-operator/pkg/k8sutil"
)

const (
	eventSourceComponent               = "update-operator"
	leaderElectionEventSourceComponent = "update-operator-leader-election"
	maxRebootingNodes                  = 1

	leaderElectionResourceName = "flatcar-linux-update-operator-lock"

	// Arbitrarily copied from KVO.
	leaderElectionLease = 90 * time.Second
	// ReconciliationPeriod.
	reconciliationPeriod = 30 * time.Second
)

var (
	// justRebootedSelector is a selector for combination of annotations
	// expected to be on a node after it has completed a reboot.
	//
	// The update-operator sets constants.AnnotationOkToReboot to true to
	// trigger a reboot, and the update-agent sets
	// constants.AnnotationRebootNeeded and
	// constants.AnnotationRebootInProgress to false when it has finished.
	justRebootedSelector = fields.Set(map[string]string{
		constants.AnnotationOkToReboot:       constants.True,
		constants.AnnotationRebootNeeded:     constants.False,
		constants.AnnotationRebootInProgress: constants.False,
	}).AsSelector()

	// rebootableSelector is a selector for the annotation expected to be on a node when it can be rebooted.
	//
	// The update-agent sets constants.AnnotationRebootNeeded to true when
	// it would like to reboot, and false when it starts up.
	//
	// If constants.AnnotationRebootPaused is set to "true", the update-agent will not consider it for rebooting.
	rebootableSelector = fields.ParseSelectorOrDie(constants.AnnotationRebootNeeded + "==" + constants.True +
		"," + constants.AnnotationRebootPaused + "!=" + constants.True +
		"," + constants.AnnotationOkToReboot + "!=" + constants.True +
		"," + constants.AnnotationRebootInProgress + "!=" + constants.True)

	// stillRebootingSelector is a selector for the annotation set expected to be
	// on a node when it's in the process of rebooting.
	stillRebootingSelector = fields.Set(map[string]string{
		constants.AnnotationOkToReboot:   constants.True,
		constants.AnnotationRebootNeeded: constants.True,
	}).AsSelector()

	// beforeRebootReq requires a node to be waiting for before reboot checks to complete.
	beforeRebootReq = k8sutil.NewRequirementOrDie(constants.LabelBeforeReboot, selection.In, []string{constants.True})

	// afterRebootReq requires a node to be waiting for after reboot checks to complete.
	afterRebootReq = k8sutil.NewRequirementOrDie(constants.LabelAfterReboot, selection.In, []string{constants.True})

	// notBeforeRebootReq and notAfterRebootReq are the inverse of the above checks.
	//
	//nolint:lll
	notBeforeRebootReq = k8sutil.NewRequirementOrDie(constants.LabelBeforeReboot, selection.NotIn, []string{constants.True})
	notAfterRebootReq  = k8sutil.NewRequirementOrDie(constants.LabelAfterReboot, selection.NotIn, []string{constants.True})
)

// Kontroller implement operator part of FLUO.
type Kontroller struct {
	kc kubernetes.Interface
	nc corev1client.NodeInterface

	// Annotations to look for before and after reboots.
	beforeRebootAnnotations []string
	afterRebootAnnotations  []string

	leaderElectionClient        *kubernetes.Clientset
	leaderElectionEventRecorder record.EventRecorder
	// Namespace is the kubernetes namespace any resources (e.g. locks,
	// configmaps, agents) should be created and read under.
	// It will be set to the namespace the operator is running in automatically.
	namespace string

	// Auto-label Flatcar Container Linux nodes for migration compatibility.
	autoLabelContainerLinux bool

	// Reboot window.
	rebootWindow *timeutil.Periodic
}

// Config configures a Kontroller.
type Config struct {
	// Kubernetes client.
	Client kubernetes.Interface
	// Migration compatibility.
	AutoLabelContainerLinux bool
	// Annotations to look for before and after reboots.
	BeforeRebootAnnotations []string
	AfterRebootAnnotations  []string
	// Reboot window.
	RebootWindowStart  string
	RebootWindowLength string
}

// New initializes a new Kontroller.
func New(config Config) (*Kontroller, error) {
	// Kubernetes client.
	if config.Client == nil {
		return nil, fmt.Errorf("kubernetes client must not be nil")
	}

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		return nil, fmt.Errorf("unable to determine operator namespace: please ensure POD_NAMESPACE " +
			"environment variable is set")
	}

	var rebootWindow *timeutil.Periodic

	if config.RebootWindowStart != "" && config.RebootWindowLength != "" {
		rw, err := timeutil.ParsePeriodic(config.RebootWindowStart, config.RebootWindowLength)
		if err != nil {
			return nil, fmt.Errorf("parsing reboot window: %w", err)
		}

		rebootWindow = rw
	}

	kc := config.Client

	// Create event emitter.
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: kc.CoreV1().Events("")})

	leaderElectionClientConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating leader election client config: %w", err)
	}

	leaderElectionClient, err := kubernetes.NewForConfig(leaderElectionClientConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating leader election client: %w", err)
	}

	leaderElectionBroadcaster := record.NewBroadcaster()
	leaderElectionBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{
		Interface: corev1client.New(leaderElectionClient.CoreV1().RESTClient()).Events(""),
	})

	leaderElectionEventRecorder := leaderElectionBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: leaderElectionEventSourceComponent,
	})

	return &Kontroller{
		kc:                          kc,
		nc:                          kc.CoreV1().Nodes(),
		beforeRebootAnnotations:     config.BeforeRebootAnnotations,
		afterRebootAnnotations:      config.AfterRebootAnnotations,
		leaderElectionClient:        leaderElectionClient,
		leaderElectionEventRecorder: leaderElectionEventRecorder,
		namespace:                   namespace,
		autoLabelContainerLinux:     config.AutoLabelContainerLinux,
		rebootWindow:                rebootWindow,
	}, nil
}

// Run starts the operator reconcilitation process and runs until the stop
// channel is closed.
func (k *Kontroller) Run(stop <-chan struct{}) error {
	if err := k.withLeaderElection(); err != nil {
		return err
	}

	// Start Flatcar Container Linux node auto-labeler.
	if k.autoLabelContainerLinux {
		go wait.Until(k.legacyLabeler, reconciliationPeriod, stop)
	}

	klog.V(5).Info("starting controller")

	// Call the process loop each period, until stop is closed.
	wait.Until(k.process, reconciliationPeriod, stop)

	klog.V(5).Info("stopping controller")

	return nil
}

// withLeaderElection creates a new context which is cancelled when this
// operator does not hold a lock to operate on the cluster.
func (k *Kontroller) withLeaderElection() error {
	// TODO: a better id might be necessary.
	// Currently, KVO uses env.POD_NAME and the upstream controller-manager uses this.
	// Both end up having the same value in general, but Hostname is
	// more likely to have a value.
	id, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("getting hostname: %w", err)
	}

	resLock := &resourcelock.ConfigMapLock{
		ConfigMapMeta: metav1.ObjectMeta{
			Namespace: k.namespace,
			Name:      leaderElectionResourceName,
		},
		Client: k.leaderElectionClient.CoreV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: k.leaderElectionEventRecorder,
		},
	}

	waitLeading := make(chan struct{})
	go func(waitLeading chan<- struct{}) {
		// Lease values inspired by a combination of
		// https://github.com/kubernetes/kubernetes/blob/f7c07a121d2afadde7aa15b12a9d02858b30a0a9/pkg/apis/componentconfig/v1alpha1/defaults.go#L163-L174
		// and the KVO values
		// See also
		// https://github.com/kubernetes/kubernetes/blob/fc31dae165f406026142f0dd9a98cada8474682a/pkg/client/leaderelection/leaderelection.go#L17
		leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
			Lock:          resLock,
			LeaseDuration: leaderElectionLease,
			//nolint:gomnd // Set renew deadline to 2/3rd of the lease duration to give
			//             // controller enough time to renew the lease.
			RenewDeadline: leaderElectionLease * 2 / 3,
			//nolint:gomnd // Retry duration is usually around 1/10th of lease duration,
			//             // but given low dynamics of FLUO, 1/3rd should also be fine.
			RetryPeriod: leaderElectionLease / 3,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) { // was: func(stop <-chan struct{
					klog.V(5).Info("started leading")
					waitLeading <- struct{}{}
				},
				OnStoppedLeading: func() {
					klog.Fatalf("leaderelection lost")
				},
			},
		})
	}(waitLeading)

	<-waitLeading

	return nil
}

// process performs the reconcilitation to coordinate reboots.
func (k *Kontroller) process() {
	klog.V(4).Info("Going through a loop cycle")

	// First make sure that all of our nodes are in a well-defined state with
	// respect to our annotations and labels, and if they are not, then try to
	// fix them.
	klog.V(4).Info("Cleaning up node state")

	if err := k.cleanupState(); err != nil {
		klog.Errorf("Failed to cleanup node state: %v", err)

		return
	}

	// Find nodes with the after-reboot=true label and check if all provided
	// annotations are set. if all annotations are set to true then remove the
	// after-reboot=true label and set reboot-ok=false, telling the agent that
	// the reboot has completed.
	klog.V(4).Info("Checking if configured after-reboot annotations are set to true")

	if err := k.checkAfterReboot(); err != nil {
		klog.Errorf("Failed to check after reboot: %v", err)

		return
	}

	// Find nodes which just rebooted but haven't run after-reboot checks.
	// remove after-reboot annotations and add the after-reboot=true label.
	klog.V(4).Info("Labeling rebooted nodes with after-reboot label")

	if err := k.markAfterReboot(); err != nil {
		klog.Errorf("Failed to update recently rebooted nodes: %v", err)

		return
	}

	// Find nodes with the before-reboot=true label and check if all provided
	// annotations are set. if all annotations are set to true then remove the
	// before-reboot=true label and set reboot=ok=true, telling the agent it's
	// time to reboot.
	klog.V(4).Info("Checking if configured before-reboot annotations are set to true")

	if err := k.checkBeforeReboot(); err != nil {
		klog.Errorf("Failed to check before reboot: %v", err)

		return
	}

	// Take some number of the rebootable nodes. remove before-reboot
	// annotations and add the before-reboot=true label.
	klog.V(4).Info("Labeling rebootable nodes with before-reboot label")

	if err := k.markBeforeReboot(); err != nil {
		klog.Errorf("Failed to update rebootable nodes: %v", err)

		return
	}
}

// cleanupState attempts to make sure nodes are in a well-defined state before
// performing state changes on them.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) cleanupState() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	for _, n := range nodelist.Items {
		err = k8sutil.UpdateNodeRetry(k.nc, n.Name, func(node *corev1.Node) {
			// Make sure that nodes with the before-reboot label actually
			// still wants to reboot.
			if _, exists := node.Labels[constants.LabelBeforeReboot]; exists {
				if !rebootableSelector.Matches(fields.Set(node.Annotations)) {
					klog.Warningf("Node %v no longer wanted to reboot while we were trying to label it so: %v",
						node.Name, node.Annotations)
					delete(node.Labels, constants.LabelBeforeReboot)
					for _, annotation := range k.beforeRebootAnnotations {
						delete(node.Annotations, annotation)
					}
				}
			}
		})
		if err != nil {
			return fmt.Errorf("cleaning up node %q: %w", n.Name, err)
		}
	}

	return nil
}

// checkReboot gets all nodes with a given requirement and checks if all of the given annotations are set to true.
//
// If they are, it deletes given annotations and label, then sets ok-to-reboot annotation to either true or false,
// depending on the given parameter.
//
// If ok-to-reboot is set to true, it gives node agent a signal that it is OK to proceed with rebooting.
//
// If ok-to-reboot is set to false, it means node has finished rebooting successfully.
//
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkReboot(req *labels.Requirement, annotations []string, label, okToReboot string) error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	nodes := k8sutil.FilterNodesByRequirement(nodelist.Items, req)

	for _, n := range nodes {
		if !hasAllAnnotations(n, annotations) {
			continue
		}

		klog.V(4).Infof("Deleting label %q for %q", label, n.Name)
		klog.V(4).Infof("Setting annotation %q to %q for %q", constants.AnnotationOkToReboot, okToReboot, n.Name)

		if err := k8sutil.UpdateNodeRetry(k.nc, n.Name, func(node *corev1.Node) {
			delete(node.Labels, label)

			// Cleanup the annotations.
			for _, annotation := range annotations {
				klog.V(4).Infof("Deleting annotation %q from node %q", annotation, node.Name)
				delete(node.Annotations, annotation)
			}

			node.Annotations[constants.AnnotationOkToReboot] = okToReboot
		}); err != nil {
			return fmt.Errorf("updating node %q: %w", n.Name, err)
		}
	}

	return nil
}

// checkBeforeReboot gets all nodes with the before-reboot=true label and checks
// if all of the configured before-reboot annotations are set to true. If they
// are, it deletes the before-reboot=true label and sets reboot-ok=true to tell
// the agent that it is ready to start the actual reboot process.
// If it goes to set reboot-ok=true and finds that the node no longer wants a
// reboot, then it just deletes the before-reboot=true label.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkBeforeReboot() error {
	return k.checkReboot(beforeRebootReq, k.beforeRebootAnnotations, constants.LabelBeforeReboot, constants.True)
}

// checkAfterReboot gets all nodes with the after-reboot=true label and checks
// if  all of the configured after-reboot annotations are set to true. If they
// are, it deletes the after-reboot=true label and sets reboot-ok=false to tell
// the agent that it has completed it's reboot successfully.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkAfterReboot() error {
	return k.checkReboot(afterRebootReq, k.beforeRebootAnnotations, constants.LabelAfterReboot, constants.False)
}

// markBeforeReboot gets nodes which want to reboot and marks them with the
// before-reboot=true label. This is considered the beginning of the reboot
// process from the perspective of the update-operator. It will only mark
// nodes with this label up to the maximum number of concurrently rebootable
// nodes as configured with the maxRebootingNodes constant. It also checks if
// we are inside the reboot window.
// It cleans up the before-reboot annotations before it applies the label, in
// case there are any left over from the last reboot.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) markBeforeReboot() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	// Check if a reboot window is configured.
	if k.rebootWindow != nil {
		// Get previous occurrence relative to now.
		period := k.rebootWindow.Previous(time.Now())
		// Check if we are inside the reboot window.
		if !(period.End.After(time.Now())) {
			klog.V(4).Info("We are outside the reboot window; not labeling rebootable nodes for now")

			return nil
		}
	}

	// Find nodes which are still rebooting.
	rebootingNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, stillRebootingSelector)
	// Nodes running before and after reboot checks are still considered to be "rebooting" to us.
	beforeRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, beforeRebootReq)
	rebootingNodes = append(rebootingNodes, beforeRebootNodes...)
	afterRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, afterRebootReq)
	rebootingNodes = append(rebootingNodes, afterRebootNodes...)

	// Verify the number of currently rebooting nodes is less than the the maximum number.
	if len(rebootingNodes) >= maxRebootingNodes {
		for _, n := range rebootingNodes {
			klog.Infof("Found node %q still rebooting, waiting", n.Name)
		}

		klog.Infof("Found %d (of max %d) rebooting nodes; waiting for completion", len(rebootingNodes), maxRebootingNodes)

		return nil
	}

	// Find nodes which want to reboot.
	rebootableNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, rebootableSelector)
	rebootableNodes = k8sutil.FilterNodesByRequirement(rebootableNodes, notBeforeRebootReq)

	// Don't even bother if rebootableNodes is empty. We wouldn't do anything anyway.
	if len(rebootableNodes) == 0 {
		return nil
	}

	// Find the number of nodes we can tell to reboot.
	remainingRebootableCount := maxRebootingNodes - len(rebootingNodes)

	// Choose some number of nodes.
	chosenNodes := make([]*corev1.Node, 0, remainingRebootableCount)
	for i := 0; i < remainingRebootableCount && i < len(rebootableNodes); i++ {
		chosenNodes = append(chosenNodes, &rebootableNodes[i])
	}

	// Set before-reboot=true for the chosen nodes.
	klog.Infof("Found %d nodes that need a reboot", len(chosenNodes))

	for _, n := range chosenNodes {
		err = k.mark(n.Name, constants.LabelBeforeReboot, "before-reboot", k.beforeRebootAnnotations)
		if err != nil {
			return fmt.Errorf("labeling node for before reboot checks: %w", err)
		}
	}

	return nil
}

// markAfterReboot gets nodes which have completed rebooting and marks them with
// the after-reboot=true label. A node with the after-reboot=true label is still
// considered to be rebooting from the perspective of the update-operator, even
// though it has completed rebooting from the machines perspective.
// It cleans up the after-reboot annotations before it applies the label, in
// case there are any left over from the last reboot.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) markAfterReboot() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	// Find nodes which just rebooted.
	justRebootedNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, justRebootedSelector)
	// Also filter out any nodes that are already labeled with after-reboot=true.
	justRebootedNodes = k8sutil.FilterNodesByRequirement(justRebootedNodes, notAfterRebootReq)

	klog.Infof("Found %d rebooted nodes", len(justRebootedNodes))

	// For all the nodes which just rebooted, remove any old annotations and add the after-reboot=true label.
	for _, n := range justRebootedNodes {
		err = k.mark(n.Name, constants.LabelAfterReboot, "after-reboot", k.afterRebootAnnotations)
		if err != nil {
			return fmt.Errorf("labeling node for after reboot checks: %w", err)
		}
	}

	return nil
}

func (k *Kontroller) mark(nodeName, label, annotationsType string, annotations []string) error {
	klog.V(4).Infof("Deleting annotations %v for %q", annotations, nodeName)
	klog.V(4).Infof("Setting label %q to %q for node %q", label, constants.True, nodeName)

	err := k8sutil.UpdateNodeRetry(k.nc, nodeName, func(node *corev1.Node) {
		for _, annotation := range annotations {
			delete(node.Annotations, annotation)
		}
		node.Labels[label] = constants.True
	})
	if err != nil {
		return fmt.Errorf("setting label %q to %q on node %q: %w", label, constants.True, nodeName, err)
	}

	if len(annotations) > 0 {
		klog.Infof("Waiting for %s annotations on node %q: %v", annotationsType, nodeName, annotations)
	}

	return nil
}

func hasAllAnnotations(node corev1.Node, annotations []string) bool {
	nodeAnnotations := node.GetAnnotations()

	for _, annotation := range annotations {
		value, ok := nodeAnnotations[annotation]
		if !ok || value != constants.True {
			return false
		}
	}

	return true
}
