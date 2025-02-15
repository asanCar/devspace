package hook

import (
	"context"
	"io"

	"github.com/loft-sh/devspace/pkg/devspace/config"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/devspace/dependency/types"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl/selector"
	logpkg "github.com/loft-sh/devspace/pkg/util/log"
	"github.com/mgutz/ansi"
)

func NewLogsHook(writer io.Writer) RemoteHook {
	return &remoteLogsHook{
		Writer: writer,
	}
}

type remoteLogsHook struct {
	Writer io.Writer
}

func (r *remoteLogsHook) ExecuteRemotely(hook *latest.HookConfig, podContainer *selector.SelectedPodContainer, client kubectl.Client, config config.Config, dependencies []types.Dependency, log logpkg.Logger) error {
	log.Infof("Execute hook '%s' in container '%s/%s/%s'", ansi.Color(hookName(hook), "white+b"), podContainer.Pod.Namespace, podContainer.Pod.Name, podContainer.Container.Name)
	reader, err := client.Logs(context.TODO(), podContainer.Pod.Namespace, podContainer.Pod.Name, podContainer.Container.Name, false, hook.Logs.TailLines, true)
	if err != nil {
		return err
	}

	_, err = io.Copy(r.Writer, reader)
	return err
}
