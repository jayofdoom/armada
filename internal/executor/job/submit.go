package job

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/G-Research/armada/internal/common/armadaerrors"
	"github.com/G-Research/armada/internal/common/util"
	"github.com/G-Research/armada/internal/executor/configuration"
	"github.com/G-Research/armada/internal/executor/context"
	"github.com/G-Research/armada/internal/executor/domain"
	"github.com/G-Research/armada/internal/executor/reporter"
	util2 "github.com/G-Research/armada/internal/executor/util"
	"github.com/G-Research/armada/pkg/api"
)

const admissionWebhookValidationFailureMessage string = "admission webhook"

type Submitter interface {
	SubmitJobs(jobsToSubmit []*api.Job) []*FailedSubmissionDetails
}

type SubmitService struct {
	eventReporter         reporter.EventReporter
	clusterContext        context.ClusterContext
	podDefaults           *configuration.PodDefaults
	submissionThreadCount int
}

func NewSubmitter(
	clusterContext context.ClusterContext,
	podDefaults *configuration.PodDefaults,
	submissionThreadCount int) *SubmitService {

	return &SubmitService{
		clusterContext:        clusterContext,
		podDefaults:           podDefaults,
		submissionThreadCount: submissionThreadCount,
	}
}

type FailedSubmissionDetails struct {
	Pod         *v1.Pod
	Job         *api.Job
	Error       error
	Recoverable bool
}

func (allocationService *SubmitService) SubmitJobs(jobsToSubmit []*api.Job) []*FailedSubmissionDetails {
	wg := &sync.WaitGroup{}
	submitJobsChannel := make(chan *api.Job)
	failedJobsChannel := make(chan *FailedSubmissionDetails, len(jobsToSubmit))

	for i := 0; i < allocationService.submissionThreadCount; i++ {
		wg.Add(1)
		go allocationService.submitWorker(wg, submitJobsChannel, failedJobsChannel)
	}

	for _, job := range jobsToSubmit {
		submitJobsChannel <- job
	}

	close(submitJobsChannel)
	wg.Wait()
	close(failedJobsChannel)

	toBeFailedJobs := make([]*FailedSubmissionDetails, 0, len(failedJobsChannel))
	for failedJob := range failedJobsChannel {
		toBeFailedJobs = append(toBeFailedJobs, failedJob)
	}

	return toBeFailedJobs
}

func (allocationService *SubmitService) submitWorker(wg *sync.WaitGroup, jobsToSubmitChannel chan *api.Job, failedJobsChannel chan *FailedSubmissionDetails) {
	defer wg.Done()

	for job := range jobsToSubmitChannel {
		jobPods := []*v1.Pod{}
		for i := range job.GetAllPodSpecs() {
			pod, err := allocationService.submitPod(job, i)
			jobPods = append(jobPods, pod)

			if err != nil {
				log.Errorf("Failed to submit job %s because %s", job.Id, err)

				// Depending on what went wrong, we may either fail the job
				// or return the lease to the scheduler to try again.
				var recoverable bool
				var e *armadaerrors.ErrCreateResource
				if status, ok := err.(k8s_errors.APIStatus); ok {
					recoverable = !isNotRecoverable(status.Status())
				} else if errors.As(err, &e) {
					recoverable = true
				} else {
					recoverable = false
				}

				errDetails := &FailedSubmissionDetails{
					Job:         job,
					Pod:         pod,
					Error:       err,
					Recoverable: recoverable,
				}

				failedJobsChannel <- errDetails

				// remove just created pods
				allocationService.clusterContext.DeletePods(jobPods)
				break
			}
		}
	}
}

// submitPod submits a pod to k8s together with any services and ingresses bundled with the Armada job.
// This function may fail partly, i.e., it may successfully create a subset of the requested objects before failing.
// In case of failure, any already created objects are not cleaned up.
func (allocationService *SubmitService) submitPod(job *api.Job, i int) (*v1.Pod, error) {

	pod := util2.CreatePod(job, allocationService.podDefaults, i)
	// Ensure the K8SService and K8SIngress fields are populated
	allocationService.populateServicesIngresses(job, pod)

	if len(job.K8SService) > 0 || len(job.K8SIngress) > 0 {
		pod.Annotations = util.MergeMaps(pod.Annotations, map[string]string{
			domain.HasIngress:               "true",
			domain.AssociatedServicesCount:  fmt.Sprintf("%d", len(job.K8SService)),
			domain.AssociatedIngressesCount: fmt.Sprintf("%d", len(job.K8SIngress)),
		})
	}

	submittedPod, err := allocationService.clusterContext.SubmitPod(pod, job.Owner, job.QueueOwnershipUserGroups)
	if err != nil {
		return pod, err
	}

	for _, service := range job.K8SService {
		service.ObjectMeta.OwnerReferences = []metav1.OwnerReference{util2.CreateOwnerReference(submittedPod)}
		_, err = allocationService.clusterContext.SubmitService(service)
		if err != nil {
			return pod, err
		}
	}

	for _, ingress := range job.K8SIngress {
		ingress.ObjectMeta.OwnerReferences = []metav1.OwnerReference{util2.CreateOwnerReference(submittedPod)}
		_, err = allocationService.clusterContext.SubmitIngress(ingress)
		if err != nil {
			return pod, err
		}
	}

	return pod, err
}

// populateServicesIngresses populates the K8SService and K8SIngress fields of the job.
// It does so by converting the Services and Ingress fields, which are Armada-specific, into proper k8s objects.
// If either of K8SService or K8SIngress is already populated (i.e., is non-nil), this function is a no-op,
// except for replacing nil-valued K8SService and K8SIngress with empty slices.
//
// TODO: I think this is wrong for jobs with multiple PodSPecs.
// Because we should create a set of services/ingresses for each pod,
// but this code is a no-op if K8SService or K8SIngress is already populated.
func (allocationService *SubmitService) populateServicesIngresses(job *api.Job, pod *v1.Pod) {
	if job.Services == nil {
		job.Services = make([]*api.ServiceConfig, 0)
	}
	if job.Ingress == nil {
		job.Ingress = make([]*api.IngressConfig, 0)
	}
	if job.K8SIngress == nil && job.K8SService == nil && exposesPorts(job, &pod.Spec) {
		k8sServices, k8sIngresses := util2.GenerateIngresses(job, pod, allocationService.podDefaults.Ingress)
		job.K8SService = k8sServices
		job.K8SIngress = k8sIngresses
	} else {
		// If K8SIngress and/or K8SService was already populated, it was populated by the Pulsar submit API.
		// Because the submit API can't know which executor the services/ingresses will be created in,
		// the executor has to provide all executor-specific information.
		for _, ingress := range job.K8SIngress {
			ingress.Annotations = util.MergeMaps(
				ingress.Annotations,
				allocationService.podDefaults.Ingress.Annotations,
			)

			// We need to use indexing here since Spec.Rules isn't pointers.
			for i, _ := range ingress.Spec.Rules {
				ingress.Spec.Rules[i].Host += allocationService.podDefaults.Ingress.HostnameSuffix
			}

			// We need to use indexing here since Spec.TLS isn't pointers.
			for i, _ := range ingress.Spec.TLS {
				ingress.Spec.TLS[i].SecretName += allocationService.podDefaults.Ingress.CertNameSuffix
			}
		}
	}
	if job.K8SService == nil {
		job.K8SService = make([]*v1.Service, 0)
	}
	if job.K8SIngress == nil {
		job.K8SIngress = make([]*networking.Ingress, 0)
	}
}

func exposesPorts(job *api.Job, podSpec *v1.PodSpec) bool {
	// This is to workaround needing to get serviceports for service configs
	// while maintaining immutability of the configs as they're passed around.
	servicesIngressConfig := util2.CombineIngressService(job.Ingress, job.Services)

	return len(util2.GetServicePorts(servicesIngressConfig, podSpec)) > 0
}

func isNotRecoverable(status metav1.Status) bool {
	if status.Reason == metav1.StatusReasonInvalid ||
		status.Reason == metav1.StatusReasonForbidden {
		return true
	}

	//This message shows it was rejected by an admission webhook.
	// By default admission webhooks blocking results in a 500 so we can't use the status code as we could confuse it with Kubernetes outage
	if strings.Contains(status.Message, admissionWebhookValidationFailureMessage) {
		return true
	}

	return false
}
