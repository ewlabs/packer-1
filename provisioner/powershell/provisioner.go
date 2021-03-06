// This package implements a provisioner for Packer that executes
// powershell scripts within the remote machine.
package powershell

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/packer/common"
	"github.com/hashicorp/packer/common/uuid"
	"github.com/hashicorp/packer/helper/config"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
)

var retryableSleep = 2 * time.Second

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// If true, the script contains binary and line endings will not be
	// converted from Windows to Unix-style.
	Binary bool

	// An inline script to execute. Multiple strings are all executed
	// in the context of a single shell.
	Inline []string

	// The local path of the powershell script to upload and execute.
	Script string

	// An array of multiple scripts to run.
	Scripts []string

	// An array of environment variables that will be injected before
	// your command(s) are executed.
	Vars []string `mapstructure:"environment_vars"`

	// The remote path where the local powershell script will be uploaded to.
	// This should be set to a writable file that is in a pre-existing directory.
	RemotePath string `mapstructure:"remote_path"`

	// The command used to execute the script. The '{{ .Path }}' variable
	// should be used to specify where the script goes, {{ .Vars }}
	// can be used to inject the environment_vars into the environment.
	ExecuteCommand string `mapstructure:"execute_command"`

	// The command used to execute the elevated script. The '{{ .Path }}' variable
	// should be used to specify where the script goes, {{ .Vars }}
	// can be used to inject the environment_vars into the environment.
	ElevatedExecuteCommand string `mapstructure:"elevated_execute_command"`

	// The timeout for retrying to start the process. Until this timeout
	// is reached, if the provisioner can't start a process, it retries.
	// This can be set high to allow for reboots.
	StartRetryTimeout time.Duration `mapstructure:"start_retry_timeout"`

	// This is used in the template generation to format environment variables
	// inside the `ExecuteCommand` template.
	EnvVarFormat string

	// This is used in the template generation to format environment variables
	// inside the `ElevatedExecuteCommand` template.
	ElevatedEnvVarFormat string `mapstructure:"elevated_env_var_format"`

	// Instructs the communicator to run the remote script as a
	// Windows scheduled task, effectively elevating the remote
	// user by impersonating a logged-in user
	ElevatedUser     string `mapstructure:"elevated_user"`
	ElevatedPassword string `mapstructure:"elevated_password"`

	// Valid Exit Codes - 0 is not always the only valid error code!
	// See http://www.symantec.com/connect/articles/windows-system-error-codes-exit-codes-description for examples
	// such as 3010 - "The requested operation is successful. Changes will not be effective until the system is rebooted."
	ValidExitCodes []int `mapstructure:"valid_exit_codes"`

	ctx interpolate.Context
}

type Provisioner struct {
	config       Config
	communicator packer.Communicator
}

type ExecuteCommandTemplate struct {
	Vars string
	Path string
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"execute_command",
				"elevated_execute_command",
			},
		},
	}, raws...)

	if err != nil {
		return err
	}

	if p.config.EnvVarFormat == "" {
		p.config.EnvVarFormat = `$env:%s="%s"; `
	}

	if p.config.ElevatedEnvVarFormat == "" {
		p.config.ElevatedEnvVarFormat = `$env:%s="%s"; `
	}

	if p.config.ExecuteCommand == "" {
		p.config.ExecuteCommand = `if (Test-Path variable:global:ProgressPreference){$ProgressPreference='SilentlyContinue'};{{.Vars}}&'{{.Path}}';exit $LastExitCode`
	}

	if p.config.ElevatedExecuteCommand == "" {
		p.config.ElevatedExecuteCommand = `if (Test-Path variable:global:ProgressPreference){$ProgressPreference='SilentlyContinue'}; . {{.Vars}}; &'{{.Path}}'; exit $LastExitCode`
	}

	if p.config.Inline != nil && len(p.config.Inline) == 0 {
		p.config.Inline = nil
	}

	if p.config.StartRetryTimeout == 0 {
		p.config.StartRetryTimeout = 5 * time.Minute
	}

	if p.config.RemotePath == "" {
		uuid := uuid.TimeOrderedUUID()
		p.config.RemotePath = fmt.Sprintf(`c:/Windows/Temp/script-%s.ps1`, uuid)
	}

	if p.config.Scripts == nil {
		p.config.Scripts = make([]string, 0)
	}

	if p.config.Vars == nil {
		p.config.Vars = make([]string, 0)
	}

	if p.config.ValidExitCodes == nil {
		p.config.ValidExitCodes = []int{0}
	}

	var errs error
	if p.config.Script != "" && len(p.config.Scripts) > 0 {
		errs = packer.MultiErrorAppend(errs,
			errors.New("Only one of script or scripts can be specified."))
	}

	if p.config.ElevatedUser != "" && p.config.ElevatedPassword == "" {
		errs = packer.MultiErrorAppend(errs,
			errors.New("Must supply an 'elevated_password' if 'elevated_user' provided"))
	}

	if p.config.ElevatedUser == "" && p.config.ElevatedPassword != "" {
		errs = packer.MultiErrorAppend(errs,
			errors.New("Must supply an 'elevated_user' if 'elevated_password' provided"))
	}

	if p.config.Script != "" {
		p.config.Scripts = []string{p.config.Script}
	}

	if len(p.config.Scripts) == 0 && p.config.Inline == nil {
		errs = packer.MultiErrorAppend(errs,
			errors.New("Either a script file or inline script must be specified."))
	} else if len(p.config.Scripts) > 0 && p.config.Inline != nil {
		errs = packer.MultiErrorAppend(errs,
			errors.New("Only a script file or an inline script can be specified, not both."))
	}

	for _, path := range p.config.Scripts {
		if _, err := os.Stat(path); err != nil {
			errs = packer.MultiErrorAppend(errs,
				fmt.Errorf("Bad script '%s': %s", path, err))
		}
	}

	// Do a check for bad environment variables, such as '=foo', 'foobar'
	for _, kv := range p.config.Vars {
		vs := strings.SplitN(kv, "=", 2)
		if len(vs) != 2 || vs[0] == "" {
			errs = packer.MultiErrorAppend(errs,
				fmt.Errorf("Environment variable not in format 'key=value': %s", kv))
		}
	}

	if errs != nil {
		return errs
	}

	return nil
}

// Takes the inline scripts, concatenates them
// into a temporary file and returns a string containing the location
// of said file.
func extractScript(p *Provisioner) (string, error) {
	temp, err := ioutil.TempFile(os.TempDir(), "packer-powershell-provisioner")
	if err != nil {
		return "", err
	}
	defer temp.Close()
	writer := bufio.NewWriter(temp)
	for _, command := range p.config.Inline {
		log.Printf("Found command: %s", command)
		if _, err := writer.WriteString(command + "\n"); err != nil {
			return "", fmt.Errorf("Error preparing powershell script: %s", err)
		}
	}

	if err := writer.Flush(); err != nil {
		return "", fmt.Errorf("Error preparing powershell script: %s", err)
	}

	return temp.Name(), nil
}

func (p *Provisioner) Provision(ui packer.Ui, comm packer.Communicator) error {
	ui.Say(fmt.Sprintf("Provisioning with Powershell..."))
	p.communicator = comm

	scripts := make([]string, len(p.config.Scripts))
	copy(scripts, p.config.Scripts)

	if p.config.Inline != nil {
		temp, err := extractScript(p)
		if err != nil {
			ui.Error(fmt.Sprintf("Unable to extract inline scripts into a file: %s", err))
		}
		scripts = append(scripts, temp)
	}

	for _, path := range scripts {
		ui.Say(fmt.Sprintf("Provisioning with powershell script: %s", path))

		log.Printf("Opening %s for reading", path)
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("Error opening powershell script: %s", err)
		}
		defer f.Close()

		command, err := p.createCommandText()
		if err != nil {
			return fmt.Errorf("Error processing command: %s", err)
		}

		// Upload the file and run the command. Do this in the context of
		// a single retryable function so that we don't end up with
		// the case that the upload succeeded, a restart is initiated,
		// and then the command is executed but the file doesn't exist
		// any longer.
		var cmd *packer.RemoteCmd
		err = p.retryable(func() error {
			if _, err := f.Seek(0, 0); err != nil {
				return err
			}
			if err := comm.Upload(p.config.RemotePath, f, nil); err != nil {
				return fmt.Errorf("Error uploading script: %s", err)
			}

			cmd = &packer.RemoteCmd{Command: command}
			return cmd.StartWithUi(comm, ui)
		})
		if err != nil {
			return err
		}

		// Close the original file since we copied it
		f.Close()

		// Check exit code against allowed codes (likely just 0)
		validExitCode := false
		for _, v := range p.config.ValidExitCodes {
			if cmd.ExitStatus == v {
				validExitCode = true
			}
		}
		if !validExitCode {
			return fmt.Errorf(
				"Script exited with non-zero exit status: %d. Allowed exit codes are: %v",
				cmd.ExitStatus, p.config.ValidExitCodes)
		}
	}

	return nil
}

func (p *Provisioner) Cancel() {
	// Just hard quit. It isn't a big deal if what we're doing keeps
	// running on the other side.
	os.Exit(0)
}

// retryable will retry the given function over and over until a
// non-error is returned.
func (p *Provisioner) retryable(f func() error) error {
	startTimeout := time.After(p.config.StartRetryTimeout)
	for {
		var err error
		if err = f(); err == nil {
			return nil
		}

		// Create an error and log it
		err = fmt.Errorf("Retryable error: %s", err)
		log.Print(err.Error())

		// Check if we timed out, otherwise we retry. It is safe to
		// retry since the only error case above is if the command
		// failed to START.
		select {
		case <-startTimeout:
			return err
		default:
			time.Sleep(retryableSleep)
		}
	}
}

func (p *Provisioner) createFlattenedEnvVars(elevated bool) (flattened string) {
	flattened = ""
	envVars := make(map[string]string)

	// Always available Packer provided env vars
	envVars["PACKER_BUILD_NAME"] = p.config.PackerBuildName
	envVars["PACKER_BUILDER_TYPE"] = p.config.PackerBuilderType
	httpAddr := common.GetHTTPAddr()
	if httpAddr != "" {
		envVars["PACKER_HTTP_ADDR"] = httpAddr
	}

	// Split vars into key/value components
	for _, envVar := range p.config.Vars {
		keyValue := strings.SplitN(envVar, "=", 2)
		envVars[keyValue[0]] = keyValue[1]
	}

	// Create a list of env var keys in sorted order
	var keys []string
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	format := p.config.EnvVarFormat
	if elevated {
		format = p.config.ElevatedEnvVarFormat
	}

	// Re-assemble vars using OS specific format pattern and flatten
	for _, key := range keys {
		flattened += fmt.Sprintf(format, key, envVars[key])
	}
	return
}

func (p *Provisioner) createCommandText() (command string, err error) {
	// Return the interpolated command
	if p.config.ElevatedUser == "" {
		return p.createCommandTextNonPrivileged()
	} else {
		return p.createCommandTextPrivileged()
	}
}

func (p *Provisioner) createCommandTextNonPrivileged() (command string, err error) {
	// Create environment variables to set before executing the command
	flattenedEnvVars := p.createFlattenedEnvVars(false)

	p.config.ctx.Data = &ExecuteCommandTemplate{
		Vars: flattenedEnvVars,
		Path: p.config.RemotePath,
	}
	command, err = interpolate.Render(p.config.ExecuteCommand, &p.config.ctx)

	if err != nil {
		return "", fmt.Errorf("Error processing command: %s", err)
	}

	commandText, err := p.generateCommandLineRunner(command)
	if err != nil {
		return "", fmt.Errorf("Error generating command line runner: %s", err)
	}

	return commandText, err
}

func (p *Provisioner) generateCommandLineRunner(command string) (commandText string, err error) {
	log.Printf("Building command line for: %s", command)

	base64EncodedCommand, err := powershellEncode(command)
	if err != nil {
		return "", fmt.Errorf("Error encoding command: %s", err)
	}

	commandText = "powershell -executionpolicy bypass -encodedCommand " + base64EncodedCommand

	return commandText, nil
}

func (p *Provisioner) createCommandTextPrivileged() (command string, err error) {
	// Can't double escape the env vars, lets create shiny new ones
	flattenedEnvVars := p.createFlattenedEnvVars(true)
	// Need to create a mini ps1 script containing all of the environment variables we want;
	// we'll be dot-sourcing this later
	envVarReader := strings.NewReader(flattenedEnvVars)
	uuid := uuid.TimeOrderedUUID()
	envVarPath := fmt.Sprintf(`${env:TEMP}\packer-env-vars-%s.ps1`, uuid)
	log.Printf("Uploading env vars to %s", envVarPath)
	err = p.communicator.Upload(envVarPath, envVarReader, nil)
	if err != nil {
		return "", fmt.Errorf("Error preparing elevated powershell script: %s", err)
	}

	p.config.ctx.Data = &ExecuteCommandTemplate{
		Path: p.config.RemotePath,
		Vars: envVarPath,
	}
	command, err = interpolate.Render(p.config.ElevatedExecuteCommand, &p.config.ctx)
	if err != nil {
		return "", fmt.Errorf("Error processing command: %s", err)
	}

	// OK so we need an elevated shell runner to wrap our command, this is going to have its own path
	// generate the script and update the command runner in the process
	path, err := p.generateElevatedRunner(command)
	if err != nil {
		return "", fmt.Errorf("Error generating elevated runner: %s", err)
	}

	// Return the path to the elevated shell wrapper
	command = fmt.Sprintf("powershell -executionpolicy bypass -file \"%s\"", path)

	return command, err
}

func (p *Provisioner) generateElevatedRunner(command string) (uploadedPath string, err error) {
	log.Printf("Building elevated command wrapper for: %s", command)

	// generate command
	var buffer bytes.Buffer

	base64EncodedCommand, err := powershellEncode(command)
	if err != nil {
		return "", fmt.Errorf("Error encoding command: %s", err)
	}

	err = elevatedTemplate.Execute(&buffer, elevatedOptions{
		User:            p.config.ElevatedUser,
		Password:        p.config.ElevatedPassword,
		TaskDescription: "Packer elevated task",
		TaskName:        fmt.Sprintf("packer-%s", uuid.TimeOrderedUUID()),
		EncodedCommand:  base64EncodedCommand,
	})

	if err != nil {
		fmt.Printf("Error creating elevated template: %s", err)
		return "", err
	}
	uuid := uuid.TimeOrderedUUID()
	path := fmt.Sprintf(`${env:TEMP}\packer-elevated-shell-%s.ps1`, uuid)
	log.Printf("Uploading elevated shell wrapper for command [%s] to [%s]", command, path)
	err = p.communicator.Upload(path, &buffer, nil)
	if err != nil {
		return "", fmt.Errorf("Error preparing elevated powershell script: %s", err)
	}

	// CMD formatted Path required for this op
	path = fmt.Sprintf("%s-%s.ps1", "%TEMP%\\packer-elevated-shell", uuid)
	return path, err
}
