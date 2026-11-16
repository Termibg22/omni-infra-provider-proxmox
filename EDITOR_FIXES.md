# Correcciones del editor e inventario: 4.1.3

La solución modifica exclusivamente el proveedor. No incluye parches, extensiones de navegador ni cambios al Omni alojado.

## Cambios de 4.1.3

- Se retiran del formulario Workload Profiles y Network Bonds. El backend conserva la lectura del YAML antiguo para evitar cambiar silenciosamente la configuración de clases existentes. Los bonds antiguos eran interfaces dentro de Talos, no cambios de red del host Proxmox.
- Cada selector incluye una primera fila vacía para deseleccionar. Se guarda una cadena vacía real, no `auto` ni un espacio. Los títulos de las demás opciones coinciden con su valor para que Omni muestre correctamente selecciones guardadas.
- Boot Disk y Primary NIC tienen encabezados independientes; también se ordenan CPU, placement, aprovisionamiento y firmware. Son separadores visuales sobre los campos planos originales, sin migrar datos ni crear otra copia de los dispositivos.
- Se retira `legacy format` de las etiquetas. Additional Disks/NICs añaden dispositivos; All Disks/NICs sustituyen la configuración principal, como indican sus ayudas.
- Se elimina el publicador periódico del esquema. `--schema-refresh-interval` se acepta por compatibilidad pero no activa ningún temporizador. La topología para los desplegables se consulta una vez al arrancar; para actualizarla hay que reiniciar el proveedor.
- Los discos adicionales ya no exigen una expresión CEL cuando se elige un almacenamiento o se hereda el del disco principal.
- Las entradas vaciadas en listas de reglas/nodos HA y grupos de firewall se omiten antes de llamar a Proxmox.

## Límites comprobados del Omni alojado

El editor original usa grupos estáticos para objetos. No expone un acordeón desde el esquema del proveedor. El selector de variantes que sí incluye JSONForms reinicializa datos al cambiar de variante, por lo que no sirve para plegar una sección conservando sus ajustes. VM Firewall Options permanece visible.

En la sesión real se observaron errores de red simultáneos en los streams de MachineClasses, InfraProviderStatuses, métricas y notificaciones. El código original de Omni vuelve a asignar el formulario al recibir otra vez el recurso MachineClass tras reconectar. El proveedor no controla esa conexión navegador-SaaS. La eliminación de su temporizador quita publicaciones periódicas de esquema, pero no arregla por sí sola esas reconexiones ni una recarga manual. No se presenta como resuelta esa pérdida de borradores.

Las listas nuevas sustituyen `auto` por la selección vacía. El backend sigue aceptando los sentinelas antiguos en YAML; si una clase antigua contiene un sentinel que el desplegable ya no ofrece, elegir la fila vacía lo reemplaza al guardar. Los valores reales como cache `none` o VGA `none` siguen siendo opciones válidas.

## Correcciones conservadas de 4.1.2

- `/nodes` determina la pertenencia actual; referencias antiguas en grupos, HA, mappings o almacenamiento no añaden nodos.
- Solo almacenamiento activo, habilitado y con `images` admite discos de VM; `rootdir` por sí solo no es suficiente. Se valida también al aprovisionar.
- Los controles de CPU, RAM, disco y NIC usan sus campos planos guardados. Los límites numéricos están dentro de subesquemas para evitar el NaN que genera el renderizador de Omni al abrir un número omitido.
- Permanecen disponibles firmware, CPU, QoS, IPs, DNS, HA, replicación, firewall, PCI/USB y templates.

El YAML agrupado de v4 sigue siendo válido en el backend, pero sus valores solo agrupados no aparecen en los controles planos; para esas clases debe editarse el manifiesto con `omnictl`.

## Verificación

- Pruebas Go de inventario con un nodo eliminado aún presente en recursos agregados, almacenamiento, grupos y mapeos.
- API simulada con un destino válido, uno `rootdir`, uno deshabilitado y otro inactivo.
- Detector de carreras, `go vet`, lint completo y compilación Linux amd64/arm64.
- Cinco pruebas de compatibilidad con el renderizador original de Omni v1.13.3, commit `1834114db70c2bbfff71cd4ba8a1882a01504193`, sin modificar sus componentes: valores antiguos visibles e intactos, formulario nuevo sin NaN/null ni dispositivos reemplazados, edición conservada durante actualización del esquema, selección/vaciado/reapertura usando el selector real de Omni, y validación del esquema al vaciar selectores o añadir discos sin CEL.
- Prueba visual local usando ese editor original y datos sintéticos; no se modificó ninguna clase ni VM real.

Para repetir las pruebas de interoperabilidad, en un checkout de Omni de esa revisión con sus dependencias de frontend instaladas, copiar `hack/test/omni-ui/provider-schema.spec.ts` a `frontend/src/components/Form/ProviderSchema.spec.ts` y ejecutar desde `frontend`:

```sh
PROXMOX_PROVIDER_SCHEMA=/ruta/absoluta/al/provider/cmd/omni-infra-provider-proxmox/data/schema.json \
  npm test -- --run src/components/Form/ProviderSchema.spec.ts
```

Ese archivo es solo una prueba: no es un parche del producto ni debe incluirse en un build de Omni.
Las pruebas reales de Proxmox siguen limitadas a lectura y simulación de API; no se ha hecho E2E con creación de VMs en el clúster del usuario.
